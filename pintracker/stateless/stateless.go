// Package stateless implements a PinTracker component for IPFS Cluster, which
// aims to reduce the memory footprint when handling really large cluster
// states.
package stateless

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/elastos/Elastos.NET.Hive.Cluster/api"
	"github.com/elastos/Elastos.NET.Hive.Cluster/pintracker/optracker"

	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	rpc "github.com/libp2p/go-libp2p-gorpc"
	peer "github.com/libp2p/go-libp2p-peer"
)

var logger = logging.Logger("pintracker")

// Tracker uses the optracker.OperationTracker to manage
// transitioning shared ipfs-cluster state (Pins) to the local IPFS node.
type Tracker struct {
	config *Config

	optracker *optracker.OperationTracker

	peerID peer.ID

	ctx    context.Context
	cancel func()

	rpcClient *rpc.Client
	rpcReady  chan struct{}

	pinCh   chan *optracker.Operation
	unpinCh chan *optracker.Operation

	shutdownMu sync.Mutex
	shutdown   bool
	wg         sync.WaitGroup
}

// New creates a new StatelessPinTracker.
func New(cfg *Config, pid peer.ID, peerName string) *Tracker {
	ctx, cancel := context.WithCancel(context.Background())

	spt := &Tracker{
		config:    cfg,
		peerID:    pid,
		ctx:       ctx,
		cancel:    cancel,
		optracker: optracker.NewOperationTracker(ctx, pid, peerName),
		rpcReady:  make(chan struct{}, 1),
		pinCh:     make(chan *optracker.Operation, cfg.MaxPinQueueSize),
		unpinCh:   make(chan *optracker.Operation, cfg.MaxPinQueueSize),
	}

	for i := 0; i < spt.config.ConcurrentPins; i++ {
		go spt.opWorker(spt.pin, spt.pinCh)
	}
	go spt.opWorker(spt.unpin, spt.unpinCh)
	return spt
}

// receives a pin Function (pin or unpin) and a channel.
// Used for both pinning and unpinning
func (spt *Tracker) opWorker(pinF func(*optracker.Operation) error, opChan chan *optracker.Operation) {
	logger.Debug("entering opworker")
	ticker := time.NewTicker(10 * time.Second) //TODO(ajl): make config var
	for {
		select {
		case <-ticker.C:
			// every tick, clear out all Done operations
			spt.optracker.CleanAllDone()
		case op := <-opChan:
			if cont := applyPinF(pinF, op); cont {
				continue
			}

			spt.optracker.Clean(op)
		case <-spt.ctx.Done():
			return
		}
	}
}

// applyPinF returns true if caller should call `continue` inside calling loop.
func applyPinF(pinF func(*optracker.Operation) error, op *optracker.Operation) bool {
	if op.Cancelled() {
		// operation was cancelled. Move on.
		// This saves some time, but not 100% needed.
		return true
	}
	op.SetPhase(optracker.PhaseInProgress)
	err := pinF(op) // call pin/unpin
	if err != nil {
		if op.Cancelled() {
			// there was an error because
			// we were cancelled. Move on.
			return true
		}
		op.SetError(err)
		op.Cancel()
		return true
	}
	op.SetPhase(optracker.PhaseDone)
	op.Cancel()
	return false
}

func (spt *Tracker) pin(op *optracker.Operation) error {
	logger.Debugf("issuing pin call for %s", op.Cid())
	err := spt.rpcClient.CallContext(
		op.Context(),
		"",
		"Cluster",
		"IPFSPin",
		op.Pin().ToSerial(),
		&struct{}{},
	)
	if err != nil {
		return err
	}
	return nil
}

func (spt *Tracker) unpin(op *optracker.Operation) error {
	logger.Debugf("issuing unpin call for %s", op.Cid())
	err := spt.rpcClient.CallContext(
		op.Context(),
		"",
		"Cluster",
		"IPFSUnpin",
		op.Pin().ToSerial(),
		&struct{}{},
	)
	if err != nil {
		return err
	}
	return nil
}

// Enqueue puts a new operation on the queue, unless ongoing exists.
func (spt *Tracker) enqueue(c api.Pin, typ optracker.OperationType) error {
	logger.Debugf("entering enqueue: pin: %+v", c)
	op := spt.optracker.TrackNewOperation(c, typ, optracker.PhaseQueued)
	if op == nil {
		return nil // ongoing pin operation.
	}

	var ch chan *optracker.Operation

	switch typ {
	case optracker.OperationPin:
		ch = spt.pinCh
	case optracker.OperationUnpin:
		ch = spt.unpinCh
	default:
		return errors.New("operation doesn't have a associated channel")
	}

	select {
	case ch <- op:
	default:
		err := errors.New("queue is full")
		op.SetError(err)
		op.Cancel()
		logger.Error(err.Error())
		return err
	}
	return nil
}

// SetClient makes the StatelessPinTracker ready to perform RPC requests to
// other components.
func (spt *Tracker) SetClient(c *rpc.Client) {
	spt.rpcClient = c
	spt.rpcReady <- struct{}{}
}

// Shutdown finishes the services provided by the StatelessPinTracker
// and cancels any active context.
func (spt *Tracker) Shutdown() error {
	spt.shutdownMu.Lock()
	defer spt.shutdownMu.Unlock()

	if spt.shutdown {
		logger.Debug("already shutdown")
		return nil
	}

	logger.Info("stopping StatelessPinTracker")
	spt.cancel()
	close(spt.rpcReady)
	spt.wg.Wait()
	spt.shutdown = true
	return nil
}

// Track tells the StatelessPinTracker to start managing a Cid,
// possibly triggering Pin operations on the IPFS daemon.
func (spt *Tracker) Track(c api.Pin) error {
	logger.Debugf("tracking %s", c.Cid)

	// Sharded pins are never pinned. A sharded pin cannot turn into
	// something else or viceversa like it happens with Remote pins so
	// we just track them.
	if c.Type == api.MetaType {
		spt.optracker.TrackNewOperation(c, optracker.OperationShard, optracker.PhaseDone)
		return nil
	}

	// Trigger unpin whenever something remote is tracked
	// Note, IPFSConn checks with pin/ls before triggering
	// pin/rm.
	if c.IsRemotePin(spt.peerID) {
		op := spt.optracker.TrackNewOperation(c, optracker.OperationRemote, optracker.PhaseInProgress)
		if op == nil {
			return nil // ongoing unpin
		}
		err := spt.unpin(op)
		op.Cancel()
		if err != nil {
			op.SetError(err)
		} else {
			op.SetPhase(optracker.PhaseDone)
		}
		return nil
	}

	return spt.enqueue(c, optracker.OperationPin)
}

// Untrack tells the StatelessPinTracker to stop managing a Cid.
// If the Cid is pinned locally, it will be unpinned.
func (spt *Tracker) Untrack(c cid.Cid) error {
	logger.Debugf("untracking %s", c)
	return spt.enqueue(api.PinCid(c), optracker.OperationUnpin)
}

// StatusAll returns information for all Cids pinned to the local IPFS node.
func (spt *Tracker) StatusAll() []api.PinInfo {
	pininfos, err := spt.localStatus(true)
	if err != nil {
		logger.Error(err)
		return nil
	}

	// get all inflight operations from optracker and
	// put them into the map, deduplicating any already 'pinned' items with
	// their inflight operation
	for _, infop := range spt.optracker.GetAll() {
		pininfos[infop.Cid.String()] = infop
	}

	var pis []api.PinInfo
	for _, pi := range pininfos {
		pis = append(pis, pi)
	}
	return pis
}

// Status returns information for a Cid pinned to the local IPFS node.
func (spt *Tracker) Status(c cid.Cid) api.PinInfo {
	// check if c has an inflight operation or errorred operation in optracker
	if oppi, ok := spt.optracker.GetExists(c); ok {
		// if it does return the status of the operation
		return oppi
	}

	// check global state to see if cluster should even be caring about
	// the provided cid
	var gpinS api.PinSerial
	err := spt.rpcClient.Call(
		"",
		"Cluster",
		"PinGet",
		api.PinCid(c).ToSerial(),
		&gpinS,
	)
	if err != nil {
		if rpc.IsRPCError(err) {
			logger.Error(err)
			return api.PinInfo{
				Cid:    c,
				Peer:   spt.peerID,
				Status: api.TrackerStatusClusterError,
				Error:  err.Error(),
				TS:     time.Now(),
			}
		}
		// not part of global state. we should not care about
		return api.PinInfo{
			Cid:    c,
			Peer:   spt.peerID,
			Status: api.TrackerStatusUnpinned,
			TS:     time.Now(),
		}
	}

	gpin := gpinS.ToPin()

	// check if pin is a meta pin
	if gpin.Type == api.MetaType {
		return api.PinInfo{
			Cid:    c,
			Peer:   spt.peerID,
			Status: api.TrackerStatusSharded,
			TS:     time.Now(),
		}
	}

	// check if pin is a remote pin
	if gpin.IsRemotePin(spt.peerID) {
		return api.PinInfo{
			Cid:    c,
			Peer:   spt.peerID,
			Status: api.TrackerStatusRemote,
			TS:     time.Now(),
		}
	}

	// else attempt to get status from ipfs node
	var ips api.IPFSPinStatus
	err = spt.rpcClient.Call(
		"",
		"Cluster",
		"IPFSPinLsCid",
		api.PinCid(c).ToSerial(),
		&ips,
	)
	if err != nil {
		logger.Error(err)
		return api.PinInfo{}
	}

	pi := api.PinInfo{
		Cid:    c,
		Peer:   spt.peerID,
		Status: ips.ToTrackerStatus(),
		TS:     time.Now(),
	}

	return pi
}

// SyncAll verifies that the statuses of all tracked Cids (from the shared state)
// match the one reported by the IPFS daemon. If not, they will be transitioned
// to PinError or UnpinError.
//
// SyncAll returns the list of local status for all tracked Cids which
// were updated or have errors. Cids in error states can be recovered
// with Recover().
// An error is returned if we are unable to contact the IPFS daemon.
func (spt *Tracker) SyncAll() ([]api.PinInfo, error) {
	// get ipfs status for all
	localpis, err := spt.localStatus(false)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	for _, p := range spt.optracker.Filter(optracker.OperationPin, optracker.PhaseError) {
		if _, ok := localpis[p.Cid.String()]; ok {
			spt.optracker.CleanError(p.Cid)
		}
	}

	for _, p := range spt.optracker.Filter(optracker.OperationUnpin, optracker.PhaseError) {
		if _, ok := localpis[p.Cid.String()]; !ok {
			spt.optracker.CleanError(p.Cid)
		}
	}

	return spt.getErrorsAll(), nil
}

// Sync returns the updated local status for the given Cid.
func (spt *Tracker) Sync(c cid.Cid) (api.PinInfo, error) {
	oppi, ok := spt.optracker.GetExists(c)
	if !ok {
		return spt.Status(c), nil
	}

	if oppi.Status == api.TrackerStatusUnpinError {
		// check global state to see if cluster should even be caring about
		// the provided cid
		var gpin api.PinSerial
		err := spt.rpcClient.Call(
			"",
			"Cluster",
			"PinGet",
			api.PinCid(c).ToSerial(),
			&gpin,
		)
		if err != nil {
			if rpc.IsRPCError(err) {
				logger.Error(err)
				return api.PinInfo{
					Cid:    c,
					Peer:   spt.peerID,
					Status: api.TrackerStatusClusterError,
					Error:  err.Error(),
					TS:     time.Now(),
				}, err
			}
			// it isn't in the global state
			spt.optracker.CleanError(c)
			return api.PinInfo{
				Cid:    c,
				Peer:   spt.peerID,
				Status: api.TrackerStatusUnpinned,
				TS:     time.Now(),
			}, nil
		}
		// check if pin is a remote pin
		if gpin.ToPin().IsRemotePin(spt.peerID) {
			spt.optracker.CleanError(c)
			return api.PinInfo{
				Cid:    c,
				Peer:   spt.peerID,
				Status: api.TrackerStatusRemote,
				TS:     time.Now(),
			}, nil
		}
	}

	if oppi.Status == api.TrackerStatusPinError {
		// else attempt to get status from ipfs node
		var ips api.IPFSPinStatus
		err := spt.rpcClient.Call(
			"",
			"Cluster",
			"IPFSPinLsCid",
			api.PinCid(c).ToSerial(),
			&ips,
		)
		if err != nil {
			logger.Error(err)
			return api.PinInfo{
				Cid:    c,
				Peer:   spt.peerID,
				Status: api.TrackerStatusPinError,
				TS:     time.Now(),
				Error:  err.Error(),
			}, err
		}
		if ips.ToTrackerStatus() == api.TrackerStatusPinned {
			spt.optracker.CleanError(c)
			pi := api.PinInfo{
				Cid:    c,
				Peer:   spt.peerID,
				Status: ips.ToTrackerStatus(),
				TS:     time.Now(),
			}
			return pi, nil
		}
	}

	return spt.optracker.Get(c), nil
}

// RecoverAll attempts to recover all items tracked by this peer.
func (spt *Tracker) RecoverAll() ([]api.PinInfo, error) {
	statuses := spt.StatusAll()
	resp := make([]api.PinInfo, 0)
	for _, st := range statuses {
		r, err := spt.Recover(st.Cid)
		if err != nil {
			return resp, err
		}
		resp = append(resp, r)
	}
	return resp, nil
}

// Recover will re-track or re-untrack a Cid in error state,
// possibly retriggering an IPFS pinning operation and returning
// only when it is done.
func (spt *Tracker) Recover(c cid.Cid) (api.PinInfo, error) {
	logger.Infof("Attempting to recover %s", c)
	pInfo, ok := spt.optracker.GetExists(c)
	if !ok {
		return spt.Status(c), nil
	}

	var err error
	switch pInfo.Status {
	case api.TrackerStatusPinError:
		err = spt.enqueue(api.PinCid(c), optracker.OperationPin)
	case api.TrackerStatusUnpinError:
		err = spt.enqueue(api.PinCid(c), optracker.OperationUnpin)
	}
	if err != nil {
		return spt.Status(c), err
	}

	return spt.Status(c), nil
}

func (spt *Tracker) ipfsStatusAll() (map[string]api.PinInfo, error) {
	var ipsMap map[string]api.IPFSPinStatus
	err := spt.rpcClient.Call(
		"",
		"Cluster",
		"IPFSPinLs",
		"recursive",
		&ipsMap,
	)
	if err != nil {
		logger.Error(err)
		return nil, err
	}
	pins := make(map[string]api.PinInfo, 0)
	for cidstr, ips := range ipsMap {
		c, err := cid.Decode(cidstr)
		if err != nil {
			logger.Error(err)
			continue
		}
		p := api.PinInfo{
			Cid:    c,
			Peer:   spt.peerID,
			Status: ips.ToTrackerStatus(),
			TS:     time.Now(),
		}
		pins[cidstr] = p
	}
	return pins, nil
}

// localStatus returns a joint set of consensusState and ipfsStatus
// marking pins which should be meta or remote and leaving any ipfs pins that
// aren't in the consensusState out.
func (spt *Tracker) localStatus(incExtra bool) (map[string]api.PinInfo, error) {
	pininfos := make(map[string]api.PinInfo)

	// get shared state
	var statePinsSerial []api.PinSerial
	err := spt.rpcClient.Call(
		"",
		"Cluster",
		"Pins",
		struct{}{},
		&statePinsSerial,
	)
	if err != nil {
		logger.Error(err)
		return nil, err
	}
	var statePins []api.Pin
	for _, p := range statePinsSerial {
		statePins = append(statePins, p.ToPin())
	}

	// get statuses from ipfs node first
	localpis, err := spt.ipfsStatusAll()
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	for _, p := range statePins {
		pCid := p.Cid.String()
		if p.Type == api.MetaType && incExtra {
			// add pin to pininfos with sharded status
			pininfos[pCid] = api.PinInfo{
				Cid:    p.Cid,
				Peer:   spt.peerID,
				Status: api.TrackerStatusSharded,
				TS:     time.Now(),
			}
			continue
		}

		if p.IsRemotePin(spt.peerID) && incExtra {
			// add pin to pininfos with a status of remote
			pininfos[pCid] = api.PinInfo{
				Cid:    p.Cid,
				Peer:   spt.peerID,
				Status: api.TrackerStatusRemote,
				TS:     time.Now(),
			}
			continue
		}
		// lookup p in localpis
		if lp, ok := localpis[pCid]; ok {
			pininfos[pCid] = lp
		}
	}
	return pininfos, nil
}

func (spt *Tracker) getErrorsAll() []api.PinInfo {
	return spt.optracker.Filter(optracker.PhaseError)
}

// OpContext exports the internal optracker's OpContext method.
// For testing purposes only.
func (spt *Tracker) OpContext(c cid.Cid) context.Context {
	return spt.optracker.OpContext(c)
}
