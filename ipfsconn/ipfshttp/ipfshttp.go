// Package ipfshttp implements an IPFS Cluster IPFSConnector component. It
// uses the IPFS HTTP API to communicate to IPFS.
package ipfshttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elastos/Elastos.NET.Hive.Cluster/api"

	cid "github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-files"
	logging "github.com/ipfs/go-log"
	rpc "github.com/libp2p/go-libp2p-gorpc"
	peer "github.com/libp2p/go-libp2p-peer"
	ma "github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
	manet "github.com/multiformats/go-multiaddr-net"
)

// DNSTimeout is used when resolving DNS multiaddresses in this module
var DNSTimeout = 5 * time.Second

var logger = logging.Logger("ipfshttp")

// updateMetricsMod only makes updates to informer metrics
// on the nth occasion. So, for example, for every BlockPut,
// only the 10th will trigger a SendInformerMetrics call.
var updateMetricMod = 10

// Connector implements the IPFSConnector interface
// and provides a component which  is used to perform
// on-demand requests against the configured IPFS daemom
// (such as a pin request).
type Connector struct {
	ctx    context.Context
	cancel func()

	config   *Config
	nodeAddr string

	rpcClient *rpc.Client
	rpcReady  chan struct{}

	client *http.Client // client to ipfs daemon

	updateMetricMutex sync.Mutex
	updateMetricCount int

	shutdownLock sync.Mutex
	shutdown     bool
	wg           sync.WaitGroup
}

type ipfsError struct {
	Message string
}

type ipfsPinType struct {
	Type string
}

type ipfsPinLsResp struct {
	Keys map[string]ipfsPinType
}

type ipfsIDResp struct {
	ID        string
	Addresses []string
}

type ipfsSwarmPeersResp struct {
	Peers []ipfsPeer
}

type ipfsPeer struct {
	Peer string
}

type ipfsStream struct {
	Protocol string
}

type ipfsKeyGenResp struct {
	Name string
	Id   string
}

type ipfsKeyRenameResp struct {
	Was       string
	Now       string
	Id        string
	Overwrite bool
}

type ipfsKeyListResp struct {
	Keys []ipfsKey
}
type ipfsKey struct {
	Name string
	Id   string
}

// NewConnector creates the component and leaves it ready to be started
func NewConnector(cfg *Config) (*Connector, error) {
	err := cfg.Validate()
	if err != nil {
		return nil, err
	}

	nodeMAddr := cfg.NodeAddr
	// dns multiaddresses need to be resolved first
	if madns.Matches(nodeMAddr) {
		ctx, cancel := context.WithTimeout(context.Background(), DNSTimeout)
		defer cancel()
		resolvedAddrs, err := madns.Resolve(ctx, cfg.NodeAddr)
		if err != nil {
			logger.Error(err)
			return nil, err
		}
		nodeMAddr = resolvedAddrs[0]
	}

	_, nodeAddr, err := manet.DialArgs(nodeMAddr)
	if err != nil {
		return nil, err
	}

	c := &http.Client{} // timeouts are handled by context timeouts

	ctx, cancel := context.WithCancel(context.Background())

	ipfs := &Connector{
		ctx:      ctx,
		config:   cfg,
		cancel:   cancel,
		nodeAddr: nodeAddr,
		rpcReady: make(chan struct{}, 1),
		client:   c,
	}

	go ipfs.run()
	return ipfs, nil
}

// connects all ipfs daemons when
// we receive the rpcReady signal.
func (ipfs *Connector) run() {
	<-ipfs.rpcReady

	// Do not shutdown while launching threads
	// -- prevents race conditions with ipfs.wg.
	ipfs.shutdownLock.Lock()
	defer ipfs.shutdownLock.Unlock()

	// This runs ipfs swarm connect to the daemons of other cluster members
	ipfs.wg.Add(1)
	go func() {
		defer ipfs.wg.Done()

		// It does not hurt to wait a little bit. i.e. think cluster
		// peers which are started at the same time as the ipfs
		// daemon...
		tmr := time.NewTimer(ipfs.config.ConnectSwarmsDelay)
		defer tmr.Stop()
		select {
		case <-tmr.C:
			// do not hang this goroutine if this call hangs
			// otherwise we hang during shutdown
			go ipfs.ConnectSwarms()
		case <-ipfs.ctx.Done():
			return
		}
	}()
}

// SetClient makes the component ready to perform RPC
// requests.
func (ipfs *Connector) SetClient(c *rpc.Client) {
	ipfs.rpcClient = c
	ipfs.rpcReady <- struct{}{}
}

// Shutdown stops any listeners and stops the component from taking
// any requests.
func (ipfs *Connector) Shutdown() error {
	ipfs.shutdownLock.Lock()
	defer ipfs.shutdownLock.Unlock()

	if ipfs.shutdown {
		logger.Debug("already shutdown")
		return nil
	}

	logger.Info("stopping IPFS Connector")

	ipfs.cancel()
	close(ipfs.rpcReady)

	ipfs.wg.Wait()
	ipfs.shutdown = true

	return nil
}

// ID performs an ID request against the configured
// IPFS daemon. It returns the fetched information.
// If the request fails, or the parsing fails, it
// returns an error and an empty IPFSID which also
// contains the error message.
func (ipfs *Connector) ID() (api.IPFSID, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	id := api.IPFSID{}
	body, err := ipfs.postCtx(ctx, "id", "", nil)
	if err != nil {
		id.Error = err.Error()
		return id, err
	}

	var res ipfsIDResp
	err = json.Unmarshal(body, &res)
	if err != nil {
		id.Error = err.Error()
		return id, err
	}

	pID, err := peer.IDB58Decode(res.ID)
	if err != nil {
		id.Error = err.Error()
		return id, err
	}
	id.ID = pID

	mAddrs := make([]ma.Multiaddr, len(res.Addresses), len(res.Addresses))
	for i, strAddr := range res.Addresses {
		mAddr, err := ma.NewMultiaddr(strAddr)
		if err != nil {
			id.Error = err.Error()
			return id, err
		}
		mAddrs[i] = mAddr
	}
	id.Addresses = mAddrs
	return id, nil
}

// Pin performs a pin request against the configured IPFS
// daemon.
func (ipfs *Connector) Pin(ctx context.Context, hash cid.Cid, maxDepth int) error {
	ctx, cancel := context.WithTimeout(ctx, ipfs.config.PinTimeout)
	defer cancel()
	pinStatus, err := ipfs.PinLsCid(ctx, hash)
	if err != nil {
		return err
	}

	if pinStatus.IsPinned(maxDepth) {
		logger.Debug("IPFS object is already pinned: ", hash)
		return nil
	}

	defer ipfs.updateInformerMetric()

	var pinArgs string
	switch {
	case maxDepth < 0:
		pinArgs = "recursive=true"
	case maxDepth == 0:
		pinArgs = "recursive=false"
	default:
		pinArgs = fmt.Sprintf("recursive=true&max-depth=%d", maxDepth)
	}

	switch ipfs.config.PinMethod {
	case "refs": // do refs -r first
		path := fmt.Sprintf("refs?arg=%s&%s", hash, pinArgs)
		err := ipfs.postDiscardBodyCtx(ctx, path)
		if err != nil {
			return err
		}
		logger.Debugf("Refs for %s sucessfully fetched", hash)
	}

	path := fmt.Sprintf("pin/add?arg=%s&%s", hash, pinArgs)
	_, err = ipfs.postCtx(ctx, path, "", nil)
	if err == nil {
		logger.Info("IPFS Pin request succeeded: ", hash)
	}
	return err
}

// Unpin performs an unpin request against the configured IPFS
// daemon.
func (ipfs *Connector) Unpin(ctx context.Context, hash cid.Cid) error {
	ctx, cancel := context.WithTimeout(ctx, ipfs.config.UnpinTimeout)
	defer cancel()

	pinStatus, err := ipfs.PinLsCid(ctx, hash)
	if err != nil {
		return err
	}
	if pinStatus.IsPinned(-1) {
		defer ipfs.updateInformerMetric()
		path := fmt.Sprintf("pin/rm?arg=%s", hash)
		_, err := ipfs.postCtx(ctx, path, "", nil)
		if err == nil {
			logger.Info("IPFS Unpin request succeeded:", hash)
		}
		return err
	}

	logger.Debug("IPFS object is already unpinned: ", hash)
	return nil
}

// PinLs performs a "pin ls --type typeFilter" request against the configured
// IPFS daemon and returns a map of cid strings and their status.
func (ipfs *Connector) PinLs(ctx context.Context, typeFilter string) (map[string]api.IPFSPinStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	body, err := ipfs.postCtx(ctx, "pin/ls?type="+typeFilter, "", nil)

	// Some error talking to the daemon
	if err != nil {
		return nil, err
	}

	var res ipfsPinLsResp
	err = json.Unmarshal(body, &res)
	if err != nil {
		logger.Error("parsing pin/ls response")
		logger.Error(string(body))
		return nil, err
	}

	statusMap := make(map[string]api.IPFSPinStatus)
	for k, v := range res.Keys {
		statusMap[k] = api.IPFSPinStatusFromString(v.Type)
	}
	return statusMap, nil
}

// PinLsCid performs a "pin ls <hash>" request. It first tries with
// "type=recursive" and then, if not found, with "type=direct". It returns an
// api.IPFSPinStatus for that hash.
func (ipfs *Connector) PinLsCid(ctx context.Context, hash cid.Cid) (api.IPFSPinStatus, error) {
	pinLsType := func(pinType string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, ipfs.config.IPFSRequestTimeout)
		defer cancel()
		lsPath := fmt.Sprintf("pin/ls?arg=%s&type=%s", hash, pinType)
		return ipfs.postCtx(ctx, lsPath, "", nil)
	}

	var body []byte
	var err error
	// FIXME: Sharding may need to check more pin types here.
	for _, pinType := range []string{"recursive", "direct"} {
		body, err = pinLsType(pinType)
		// Network error, daemon down
		if body == nil && err != nil {
			return api.IPFSPinStatusError, err
		}

		// Pin found. Do not keep looking.
		if err == nil {
			break
		}
	}

	if err != nil { // we could not find the pin
		return api.IPFSPinStatusUnpinned, nil
	}

	var res ipfsPinLsResp
	err = json.Unmarshal(body, &res)
	if err != nil {
		logger.Error("parsing pin/ls?arg=cid response:")
		logger.Error(string(body))
		return api.IPFSPinStatusError, err
	}
	pinObj, ok := res.Keys[hash.String()]
	if !ok {
		return api.IPFSPinStatusError, errors.New("expected to find the pin in the response")
	}

	return api.IPFSPinStatusFromString(pinObj.Type), nil
}

func (ipfs *Connector) doPostCtx(ctx context.Context, client *http.Client, apiURL, path string, contentType string, postBody io.Reader) (*http.Response, error) {
	logger.Debugf("posting %s", path)
	urlstr := fmt.Sprintf("%s/%s", apiURL, path)

	req, err := http.NewRequest("POST", urlstr, postBody)
	if err != nil {
		logger.Error("error creating POST request:", err)
	}

	req.Header.Set("Content-Type", contentType)
	req = req.WithContext(ctx)
	res, err := ipfs.client.Do(req)
	if err != nil {
		logger.Error("error posting to IPFS:", err)
	}

	return res, err
}

// checkResponse tries to parse an error message on non StatusOK responses
// from ipfs.
func checkResponse(path string, code int, body []byte) error {
	if code == http.StatusOK {
		return nil
	}

	var ipfsErr ipfsError

	if body != nil && json.Unmarshal(body, &ipfsErr) == nil {
		return fmt.Errorf("IPFS unsuccessful: %d: %s", code, ipfsErr.Message)
	}
	// No error response with useful message from ipfs
	return fmt.Errorf("IPFS-post '%s' unsuccessful: %d: %s", path, code, body)
}

// postCtx makes a POST request against
// the ipfs daemon, reads the full body of the response and
// returns it after checking for errors.
func (ipfs *Connector) postCtx(ctx context.Context, path string, contentType string, postBody io.Reader) ([]byte, error) {
	res, err := ipfs.doPostCtx(ctx, ipfs.client, ipfs.apiURL(), path, contentType, postBody)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		logger.Errorf("error reading response body: %s", err)
		return nil, err
	}
	return body, checkResponse(path, res.StatusCode, body)
}

// postDiscardBodyCtx makes a POST requests but discards the body
// of the response directly after reading it.
func (ipfs *Connector) postDiscardBodyCtx(ctx context.Context, path string) error {
	res, err := ipfs.doPostCtx(ctx, ipfs.client, ipfs.apiURL(), path, "", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, err = io.Copy(ioutil.Discard, res.Body)
	if err != nil {
		return err
	}
	return checkResponse(path, res.StatusCode, nil)
}

// apiURL is a short-hand for building the url of the IPFS
// daemon API.
func (ipfs *Connector) apiURL() string {
	return fmt.Sprintf("http://%s/api/v0", ipfs.nodeAddr)
}

// ConnectSwarms requests the ipfs addresses of other peers and
// triggers ipfs swarm connect requests
func (ipfs *Connector) ConnectSwarms() error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	idsSerial := make([]api.IDSerial, 0)
	err := ipfs.rpcClient.Call(
		"",
		"Cluster",
		"Peers",
		struct{}{},
		&idsSerial,
	)
	if err != nil {
		logger.Error(err)
		return err
	}
	logger.Debugf("%+v", idsSerial)

	for _, idSerial := range idsSerial {
		ipfsID := idSerial.IPFS
		for _, addr := range ipfsID.Addresses {
			// This is a best effort attempt
			// We ignore errors which happens
			// when passing in a bunch of addresses
			_, err := ipfs.postCtx(
				ctx,
				fmt.Sprintf("swarm/connect?arg=%s", addr),
				"",
				nil,
			)
			if err != nil {
				logger.Debug(err)
				continue
			}
			logger.Debugf("ipfs successfully connected to %s", addr)
		}
	}
	return nil
}

// ConfigKey fetches the IPFS daemon configuration and retrieves the value for
// a given configuration key. For example, "Datastore/StorageMax" will return
// the value for StorageMax in the Datastore configuration object.
func (ipfs *Connector) ConfigKey(keypath string) (interface{}, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	res, err := ipfs.postCtx(ctx, "config/show", "", nil)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	var cfg map[string]interface{}
	err = json.Unmarshal(res, &cfg)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	path := strings.SplitN(keypath, "/", 2)
	if len(path) == 0 {
		return nil, errors.New("cannot lookup without a path")
	}

	return getConfigValue(path, cfg)
}

func getConfigValue(path []string, cfg map[string]interface{}) (interface{}, error) {
	value, ok := cfg[path[0]]
	if !ok {
		return nil, errors.New("key not found in configuration")
	}

	if len(path) == 1 {
		return value, nil
	}

	switch value.(type) {
	case map[string]interface{}:
		v := value.(map[string]interface{})
		return getConfigValue(path[1:], v)
	default:
		return nil, errors.New("invalid path")
	}
}

// RepoStat returns the DiskUsage and StorageMax repo/stat values from the
// ipfs daemon, in bytes, wrapped as an IPFSRepoStat object.
func (ipfs *Connector) RepoStat() (api.IPFSRepoStat, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	res, err := ipfs.postCtx(ctx, "repo/stat?size-only=true", "", nil)
	if err != nil {
		logger.Error(err)
		return api.IPFSRepoStat{}, err
	}

	var stats api.IPFSRepoStat
	err = json.Unmarshal(res, &stats)
	if err != nil {
		logger.Error(err)
		return stats, err
	}
	return stats, nil
}

// SwarmPeers returns the peers currently connected to this ipfs daemon.
func (ipfs *Connector) SwarmPeers() (api.SwarmPeers, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	swarm := api.SwarmPeers{}
	res, err := ipfs.postCtx(ctx, "swarm/peers", "", nil)
	if err != nil {
		logger.Error(err)
		return swarm, err
	}
	var peersRaw ipfsSwarmPeersResp
	err = json.Unmarshal(res, &peersRaw)
	if err != nil {
		logger.Error(err)
		return swarm, err
	}

	swarm = make([]peer.ID, len(peersRaw.Peers))
	for i, p := range peersRaw.Peers {
		pID, err := peer.IDB58Decode(p.Peer)
		if err != nil {
			logger.Error(err)
			return swarm, err
		}
		swarm[i] = pID
	}
	return swarm, nil
}

// BlockPut triggers an ipfs block put on the given data, inserting the block
// into the ipfs daemon's repo.
func (ipfs *Connector) BlockPut(b api.NodeWithMeta) error {
	logger.Debugf("putting block to IPFS: %s", b.Cid)
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	defer ipfs.updateInformerMetric()

	mapDir := files.NewMapDirectory(
		map[string]files.Node{ // IPFS reqs require a wrapping directory
			"": files.NewBytesFile(b.Data),
		},
	)

	multiFileR := files.NewMultiFileReader(mapDir, true)
	if b.Format == "" {
		b.Format = "v0"
	}
	url := "block/put?f=" + b.Format
	contentType := "multipart/form-data; boundary=" + multiFileR.Boundary()

	_, err := ipfs.postCtx(ctx, url, contentType, multiFileR)
	return err
}

// BlockGet retrieves an ipfs block with the given cid
func (ipfs *Connector) BlockGet(c cid.Cid) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "block/get?arg=" + c.String()
	return ipfs.postCtx(ctx, url, "", nil)
}

// Returns true every updateMetricsMod-th time that we
// call this function.
func (ipfs *Connector) shouldUpdateMetric() bool {
	ipfs.updateMetricMutex.Lock()
	defer ipfs.updateMetricMutex.Unlock()
	ipfs.updateMetricCount++
	if ipfs.updateMetricCount%updateMetricMod == 0 {
		ipfs.updateMetricCount = 0
		return true
	}
	return false
}

// Trigger a broadcast of the local informer metrics.
func (ipfs *Connector) updateInformerMetric() error {
	if !ipfs.shouldUpdateMetric() {
		return nil
	}

	var metric api.Metric

	err := ipfs.rpcClient.GoContext(
		ipfs.ctx,
		"",
		"Cluster",
		"SendInformerMetric",
		struct{}{},
		&metric,
		nil,
	)
	if err != nil {
		logger.Error(err)
	}
	return err
}

type hiveErrorString struct {
	s string
}

func (e *hiveErrorString) Error() string {
	return e.s
}

func hiveError(err error, uid string) error {
	e := err.Error()
	return &hiveErrorString{strings.Replace(e, "/nodes/"+uid, "", -1)}
}

// create a virtual id.
func (ipfs *Connector) UidNew(name string) (api.UIDSecret, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	secret := api.UIDSecret{}
	url := "key/gen?arg=" + name + "&type=rsa"
	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	url = "files/mkdir?arg=/nodes/" + name + "&parents=true"
	_, err = ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	var keyGen ipfsKeyGenResp
	err = json.Unmarshal(res, &keyGen)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	secret.UID = keyGen.Name
	secret.PeerID = keyGen.Id

	return secret, nil
}

// log in Hive cluster and get new id
func (ipfs *Connector) UidRenew(l []string) (api.UIDRenew, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	secret := api.UIDRenew{}
	url := "key/rename?arg=" + l[0] + "&arg=" + l[1]
	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	url = "files/mv?arg=/nodes/" + l[0] + "&arg=/nodes/" + l[1]
	_, err = ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	var keyRename ipfsKeyRenameResp
	err = json.Unmarshal(res, &keyRename)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	secret.UID = keyRename.Now
	secret.OldUID = keyRename.Was
	secret.PeerID = keyRename.Id

	return secret, nil
}

// log in Hive cluster and get new id
func (ipfs *Connector) UidInfo(uid string) (api.UIDSecret, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()

	secret := api.UIDSecret{}
	url := "key/list"
	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	var keyList ipfsKeyListResp
	err = json.Unmarshal(res, &keyList)
	if err != nil {
		logger.Error(err)
		return secret, err
	}

	for _, key := range keyList.Keys {
		if key.Name == uid {
			secret.UID = key.Name
			secret.PeerID = key.Id
			break
		}
	}

	return secret, nil
}

// log in Hive cluster to recreate user home
func (ipfs *Connector) UidLogin(params []string) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()

	uid := params[0]
	hash := params[1]
	if !strings.HasPrefix(hash, "/ipfs/") {
		hash = "/ipfs/" + hash
	}

	url := "files/rm?arg=/nodes/" + uid + "&recursive=true&force=true"
	_, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
	}

	url = "files/cp?arg=" + hash + "&arg=" + "/nodes/" + uid
	_, err = ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return hiveError(err, uid)
	}

	return nil
}

// get file from IPFS service
func (ipfs *Connector) FileGet(fg []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()

	url := "get?arg=" + fg[0]

	if fg[1] != "" {
		url = url + "&output=" + fg[1]
	}
	if fg[2] != "" {
		url = url + "&archive=" + fg[2]
	}
	if fg[3] != "" {
		url = url + "&compress=" + fg[3]
	}
	if fg[4] != "" {
		url = url + "&compression-level=" + fg[4]
	}

	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	return res, nil
}

// copy file to Hive
func (ipfs *Connector) FilesCp(l []string) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/cp?arg=" + l[1] + "&arg=" + filepath.Join("/nodes/", l[0], l[2])
	_, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return hiveError(err, l[0])
	}

	return nil
}

// file flushs
func (ipfs *Connector) FilesFlush(l []string) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/flush?arg=" + filepath.Join("/nodes/", l[0], l[1])

	_, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return hiveError(err, l[0])
	}

	return nil
}

// list file or directory
func (ipfs *Connector) FilesLs(l []string) (api.FilesLs, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/ls?arg=" + filepath.Join("/nodes/", l[0], l[1])
	lsrsp := api.FilesLs{}

	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return lsrsp, hiveError(err, l[0])
	}

	err = json.Unmarshal(res, &lsrsp)
	if err != nil {
		logger.Error(err)
		return lsrsp, err
	}

	return lsrsp, nil
}

// create a directotry
func (ipfs *Connector) FilesMkdir(mk []string) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/mkdir?arg=" + filepath.Join("/nodes/", mk[0], mk[1]) + "&parents=" + mk[2]

	_, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return hiveError(err, mk[0])
	}

	return nil
}

// move files
func (ipfs *Connector) FilesMv(mv []string) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/mv?arg=" + filepath.Join("/nodes/", mv[0], mv[1]) + "&arg=" + filepath.Join("/nodes/", mv[0], mv[2])

	_, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return hiveError(err, mv[0])
	}

	return nil
}

// read file
func (ipfs *Connector) FilesRead(l []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/read?arg=" + filepath.Join("/nodes/", l[0], l[1])
	if l[2] != "" {
		url = url + "&offset=" + l[2]
	}
	if l[3] != "" {
		url = url + "&count=" + l[3]
	}

	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return nil, hiveError(err, l[0])
	}

	return res, nil
}

// remove file
func (ipfs *Connector) FilesRm(rm []string) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()
	url := "files/rm?arg=" + filepath.Join("/nodes/", rm[0], rm[1]) + "&recursive=" + rm[2]

	_, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return hiveError(err, rm[0])
	}

	return nil
}

// get file statistic
func (ipfs *Connector) FilesStat(st []string) (api.FilesStat, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()

	FilesStat := api.FilesStat{}
	url := "files/stat?arg=" + filepath.Join("/nodes/", st[0], st[1])

	if st[2] != "" {
		url = url + "&format=" + st[2]
	}
	if st[3] != "" {
		url = url + "&hash=" + st[3]
	}
	if st[4] != "" {
		url = url + "&size=" + st[4]
	}
	if st[5] != "" {
		url = url + "&with-local=" + st[5]
	}

	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return FilesStat, hiveError(err, st[0])
	}

	err = json.Unmarshal(res, &FilesStat)
	if err != nil {
		logger.Error(err)
		return FilesStat, err
	}

	return FilesStat, nil
}

// write file
func (ipfs *Connector) FilesWrite(fr api.FilesWrite) error {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()

	url := "files/write?arg=" + filepath.Join("/nodes/", fr.Params[0], fr.Params[1])

	if fr.Params[2] != "" {
		url = url + "&offset=" + fr.Params[2]
	}
	if fr.Params[3] != "" {
		url = url + "&create=" + fr.Params[3]
	}
	if fr.Params[4] != "" {
		url = url + "&truncate=" + fr.Params[4]
	}
	if fr.Params[5] != "" {
		url = url + "&count=" + fr.Params[5]
	}
	if fr.Params[6] != "" {
		url = url + "&raw-leaves=" + fr.Params[6]
	}
	if fr.Params[7] != "" {
		url = url + "&cid-version=" + fr.Params[7]
	}
	if fr.Params[8] != "" {
		url = url + "&hash=" + fr.Params[8]
	}

	_, err := ipfs.postCtx(ctx, url, fr.ContentType, fr.BodyBuf)
	if err != nil {
		logger.Error(err)
		return hiveError(err, fr.Params[0])
	}

	return nil
}

// NamePublish publish ipfs path with uid
func (ipfs *Connector) NamePublish(np []string) (api.NamePublish, error) {
	ctx, cancel := context.WithTimeout(ipfs.ctx, ipfs.config.IPFSRequestTimeout)
	defer cancel()

	NamePublish := api.NamePublish{}
	url := "name/publish?arg=" + np[1] + "&key=" + np[0]

	if np[2] != "" {
		url = url + "&lifetime=" + np[2]
	}

	res, err := ipfs.postCtx(ctx, url, "", nil)
	if err != nil {
		logger.Error(err)
		return NamePublish, err
	}

	err = json.Unmarshal(res, &NamePublish)
	if err != nil {
		logger.Error(err)
		return NamePublish, err
	}

	return NamePublish, nil
}
