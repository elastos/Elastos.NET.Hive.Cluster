package ipfsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elastos/Elastos.NET.Hive.Cluster/adder/adderutils"
	"github.com/elastos/Elastos.NET.Hive.Cluster/api"
	"github.com/elastos/Elastos.NET.Hive.Cluster/rpcutil"

	mux "github.com/gorilla/mux"
	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	rpc "github.com/libp2p/go-libp2p-gorpc"
	peer "github.com/libp2p/go-libp2p-peer"
	madns "github.com/multiformats/go-multiaddr-dns"
	manet "github.com/multiformats/go-multiaddr-net"
	uuid "github.com/satori/go.uuid"
	tar "github.com/whyrusleeping/tar-utils"
)

// DNSTimeout is used when resolving DNS multiaddresses in this module
var DNSTimeout = 5 * time.Second

var logger = logging.Logger("ipfsproxy")

// Server offers an IPFS API, hijacking some interesting requests
// and forwarding the rest to the ipfs daemon
// it proxies HTTP requests to the configured IPFS
// daemon. It is able to intercept these requests though, and
// perform extra operations on them.
type Server struct {
	ctx    context.Context
	cancel func()

	config     *Config
	nodeScheme string
	nodeAddr   string

	rpcClient *rpc.Client
	rpcReady  chan struct{}

	listener         net.Listener      // proxy listener
	server           *http.Server      // proxy server
	ipfsRoundTripper http.RoundTripper // allows to talk to IPFS

	ipfsHeadersStore sync.Map

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

type ipfsPinOpResp struct {
	Pins []string
}

// From https://github.com/ipfs/go-ipfs/blob/master/core/coreunix/add.go#L49
type ipfsAddResp struct {
	Name  string
	Hash  string `json:",omitempty"`
	Bytes int64  `json:",omitempty"`
	Size  string `json:",omitempty"`
}

type ipfsUidNewResp struct {
	UID    string
	PeerID string
}

type ipfsUidRenewResp struct {
	OldUID string
	UID    string
	PeerID string
}

// New returns and ipfs Proxy component
func New(cfg *Config) (*Server, error) {
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

	proxyNet, proxyAddr, err := manet.DialArgs(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}

	l, err := net.Listen(proxyNet, proxyAddr)
	if err != nil {
		return nil, err
	}

	nodeScheme := "http"
	if cfg.NodeHTTPS {
		nodeScheme = "https"
	}
	nodeHTTPAddr := fmt.Sprintf("%s://%s", nodeScheme, nodeAddr)
	proxyURL, err := url.Parse(nodeHTTPAddr)
	if err != nil {
		return nil, err
	}

	router := mux.NewRouter()
	s := &http.Server{
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		Handler:           router,
	}

	// See: https://github.com/ipfs/go-ipfs/issues/5168
	// See: https://github.com/ipfs/ipfs-cluster/issues/548
	// on why this is re-enabled.
	s.SetKeepAlivesEnabled(true) // A reminder that this can be changed

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)
	reverseProxy.Transport = http.DefaultTransport
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &Server{
		ctx:              ctx,
		config:           cfg,
		cancel:           cancel,
		nodeAddr:         nodeHTTPAddr,
		nodeScheme:       nodeScheme,
		rpcReady:         make(chan struct{}, 1),
		listener:         l,
		server:           s,
		ipfsRoundTripper: reverseProxy.Transport,
	}

	// Ideally, we should only intercept POST requests, but
	// people may be calling the API with GET or worse, PUT
	// because IPFS has been allowing this traditionally.
	// The main idea here is that we do not intercept
	// OPTIONS requests (or HEAD).
	hijackSubrouter := router.
		Methods(http.MethodPost, http.MethodGet, http.MethodPut).
		PathPrefix("/api/v0").
		Subrouter()

	// Add hijacked routes
	hijackSubrouter.
		Path("/pin/add/{arg}").
		HandlerFunc(slashHandler(proxy.pinHandler)).
		Name("PinAddSlash") // supports people using the API wrong.
	hijackSubrouter.
		Path("/pin/add").
		HandlerFunc(proxy.pinHandler).
		Name("PinAdd")
	hijackSubrouter.
		Path("/pin/rm/{arg}").
		HandlerFunc(slashHandler(proxy.unpinHandler)).
		Name("PinRmSlash") // supports people using the API wrong.
	hijackSubrouter.
		Path("/pin/rm").
		HandlerFunc(proxy.unpinHandler).
		Name("PinRm")
	hijackSubrouter.
		Path("/pin/ls/{arg}").
		HandlerFunc(slashHandler(proxy.pinLsHandler)).
		Name("PinLsSlash") // supports people using the API wrong.
	hijackSubrouter.
		Path("/pin/ls").
		HandlerFunc(proxy.pinLsHandler).
		Name("PinLs")
	hijackSubrouter.
		Path("/add").
		HandlerFunc(proxy.addHandler).
		Name("Add")
	hijackSubrouter.
		Path("/repo/stat").
		HandlerFunc(proxy.repoStatHandler).
		Name("RepoStat")

	hijackSubrouter.
		Path("/uid/new").
		HandlerFunc(proxy.uidNewHandler).
		Name("UidNew")
	hijackSubrouter.
		Path("/uid/renew").
		HandlerFunc(proxy.uidRenewHandler).
		Name("UidRenew")
	hijackSubrouter.
		Path("/uid/info").
		HandlerFunc(proxy.uidInfoHandler).
		Name("UidInfo")
	hijackSubrouter.
		Path("/uid/login").
		HandlerFunc(proxy.uidLoginHandler).
		Name("UidLogin")

	hijackSubrouter.
		Path("/file/add").
		HandlerFunc(proxy.addHandler).
		Name("FileAdd")
	hijackSubrouter.
		Path("/file/get").
		HandlerFunc(proxy.fileGetHandler).
		Name("FileGet")
	hijackSubrouter.
		Path("/file/cat").
		HandlerFunc(proxy.fileCatHandler).
		Name("FileCat")
	// pass throught
	// hijackSubrouter.
	// 	Path("/file/ls").
	// 	HandlerFunc(proxy.fileLsHandler).
	// 	Name("FileLs")

	hijackSubrouter.
		Path("/files/cp").
		HandlerFunc(proxy.filesCpHandler).
		Name("FilesCp")
	hijackSubrouter.
		Path("/files/flush").
		HandlerFunc(proxy.filesFlushHandler).
		Name("FilesFlush")
	hijackSubrouter.
		Path("/files/ls").
		HandlerFunc(proxy.filesLsHandler).
		Name("FilesLs")
	hijackSubrouter.
		Path("/files/mkdir").
		HandlerFunc(proxy.filesMkdirHandler).
		Name("FilesMkdir")
	hijackSubrouter.
		Path("/files/mv").
		HandlerFunc(proxy.filesMvHandler).
		Name("FilesMv")
	hijackSubrouter.
		Path("/files/read").
		HandlerFunc(proxy.filesReadHandler).
		Name("FilesRead")
	hijackSubrouter.
		Path("/files/rm").
		HandlerFunc(proxy.filesRmHandler).
		Name("FilesRm")
	hijackSubrouter.
		Path("/files/stat").
		HandlerFunc(proxy.filesStatHandler).
		Name("FileStat")
	hijackSubrouter.
		Path("/files/write").
		HandlerFunc(proxy.filesWriteHandler).
		Name("FileWrite")

	hijackSubrouter.
		Path("/name/publish").
		HandlerFunc(proxy.namePublishHandler).
		Name("NamePublish")

	// Everything else goes to the IPFS daemon.
	router.PathPrefix("/").Handler(reverseProxy)

	go proxy.run()
	return proxy, nil
}

// SetClient makes the component ready to perform RPC
// requests.
func (proxy *Server) SetClient(c *rpc.Client) {
	proxy.rpcClient = c
	proxy.rpcReady <- struct{}{}
}

// Shutdown stops any listeners and stops the component from taking
// any requests.
func (proxy *Server) Shutdown() error {
	proxy.shutdownLock.Lock()
	defer proxy.shutdownLock.Unlock()

	if proxy.shutdown {
		logger.Debug("already shutdown")
		return nil
	}

	logger.Info("stopping IPFS Proxy")

	proxy.cancel()
	close(proxy.rpcReady)
	proxy.server.SetKeepAlivesEnabled(false)
	proxy.listener.Close()

	proxy.wg.Wait()
	proxy.shutdown = true
	return nil
}

// launches proxy when we receive the rpcReady signal.
func (proxy *Server) run() {
	<-proxy.rpcReady

	// Do not shutdown while launching threads
	// -- prevents race conditions with proxy.wg.
	proxy.shutdownLock.Lock()
	defer proxy.shutdownLock.Unlock()

	// This launches the proxy
	proxy.wg.Add(1)
	go func() {
		defer proxy.wg.Done()
		logger.Infof(
			"IPFS Proxy: %s -> %s",
			proxy.config.ListenAddr,
			proxy.config.NodeAddr,
		)
		err := proxy.server.Serve(proxy.listener) // hangs here
		if err != nil && !strings.Contains(err.Error(), "closed network connection") {
			logger.Error(err)
		}
	}()
}

// ipfsErrorResponder writes an http error response just like IPFS would.
func ipfsErrorResponder(w http.ResponseWriter, errMsg string) {
	res := ipfsError{errMsg}
	resBytes, _ := json.Marshal(res)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write(resBytes)
	return
}

func (proxy *Server) pinOpHandler(op string, w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	arg := r.URL.Query().Get("arg")
	c, err := cid.Decode(arg)
	if err != nil {
		ipfsErrorResponder(w, "Error parsing CID: "+err.Error())
		return
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		op,
		api.PinCid(c).ToSerial(),
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	res := ipfsPinOpResp{
		Pins: []string{arg},
	}
	resBytes, _ := json.Marshal(res)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

func (proxy *Server) pinHandler(w http.ResponseWriter, r *http.Request) {
	proxy.pinOpHandler("Pin", w, r)
}

func (proxy *Server) unpinHandler(w http.ResponseWriter, r *http.Request) {
	proxy.pinOpHandler("Unpin", w, r)
}

func (proxy *Server) pinLsHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	pinLs := ipfsPinLsResp{}
	pinLs.Keys = make(map[string]ipfsPinType)

	arg := r.URL.Query().Get("arg")
	if arg != "" {
		c, err := cid.Decode(arg)
		if err != nil {
			ipfsErrorResponder(w, err.Error())
			return
		}
		var pin api.PinSerial
		err = proxy.rpcClient.Call(
			"",
			"Cluster",
			"PinGet",
			api.PinCid(c).ToSerial(),
			&pin,
		)
		if err != nil {
			ipfsErrorResponder(w, fmt.Sprintf("Error: path '%s' is not pinned", arg))
			return
		}
		pinLs.Keys[pin.Cid] = ipfsPinType{
			Type: "recursive",
		}
	} else {
		pins := make([]api.PinSerial, 0)
		err := proxy.rpcClient.Call(
			"",
			"Cluster",
			"Pins",
			struct{}{},
			&pins,
		)
		if err != nil {
			ipfsErrorResponder(w, err.Error())
			return
		}

		for _, pin := range pins {
			pinLs.Keys[pin.Cid] = ipfsPinType{
				Type: "recursive",
			}
		}
	}

	resBytes, _ := json.Marshal(pinLs)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
}

func (proxy *Server) addHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	reader, err := r.MultipartReader()
	if err != nil {
		ipfsErrorResponder(w, "error reading request: "+err.Error())
		return
	}

	q := r.URL.Query()
	if q.Get("only-hash") == "true" {
		ipfsErrorResponder(w, "only-hash is not supported when adding to cluster")
	}

	unpin := q.Get("pin") == "false"

	// Luckily, most IPFS add query params are compatible with cluster's
	// /add params. We can parse most of them directly from the query.
	params, err := api.AddParamsFromQuery(q)
	if err != nil {
		ipfsErrorResponder(w, "error parsing options:"+err.Error())
		return
	}
	trickle := q.Get("trickle")
	if trickle == "true" {
		params.Layout = "trickle"
	}

	logger.Warningf("Proxy/add does not support all IPFS params. Current options: %+v", params)

	outputTransform := func(in *api.AddedOutput) interface{} {
		r := &ipfsAddResp{
			Name:  in.Name,
			Hash:  in.Cid,
			Bytes: int64(in.Bytes),
		}
		if in.Size != 0 {
			r.Size = strconv.FormatUint(in.Size, 10)
		}
		return r
	}

	root, err := adderutils.AddMultipartHTTPHandler(
		proxy.ctx,
		proxy.rpcClient,
		params,
		reader,
		w,
		outputTransform,
	)

	// any errors have been sent as Trailer
	if err != nil {
		return
	}

	if !unpin {
		return
	}

	// Unpin because the user doesn't want to pin
	time.Sleep(100 * time.Millisecond)
	err = proxy.rpcClient.CallContext(
		proxy.ctx,
		"",
		"Cluster",
		"Unpin",
		api.PinCid(root).ToSerial(),
		&struct{}{},
	)
	if err != nil {
		w.Header().Set("X-Stream-Error", err.Error())
		return
	}
}

func (proxy *Server) repoStatHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	peers := make([]peer.ID, 0)
	err := proxy.rpcClient.Call(
		"",
		"Cluster",
		"ConsensusPeers",
		struct{}{},
		&peers,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	ctxs, cancels := rpcutil.CtxsWithCancel(proxy.ctx, len(peers))
	defer rpcutil.MultiCancel(cancels)

	repoStats := make([]api.IPFSRepoStat, len(peers), len(peers))
	repoStatsIfaces := make([]interface{}, len(repoStats), len(repoStats))
	for i := range repoStats {
		repoStatsIfaces[i] = &repoStats[i]
	}

	errs := proxy.rpcClient.MultiCall(
		ctxs,
		peers,
		"Cluster",
		"IPFSRepoStat",
		struct{}{},
		repoStatsIfaces,
	)

	totalStats := api.IPFSRepoStat{}

	for i, err := range errs {
		if err != nil {
			logger.Errorf("%s repo/stat errored: %s", peers[i], err)
			continue
		}
		totalStats.RepoSize += repoStats[i].RepoSize
		totalStats.StorageMax += repoStats[i].StorageMax
	}

	resBytes, _ := json.Marshal(totalStats)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

// slashHandler returns a handler which converts a /a/b/c/<argument> request
// into an /a/b/c/<argument>?arg=<argument> one. And uses the given origHandler
// for it. Our handlers expect that arguments are passed in the ?arg query
// value.
func slashHandler(origHandler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		warnMsg := "You are using an undocumented form of the IPFS API. "
		warnMsg += "Consider passing your command arguments"
		warnMsg += "with the '?arg=' query parameter"
		logger.Error(warnMsg)

		vars := mux.Vars(r)
		arg := vars["arg"]

		// IF we needed to modify the request path, we could do
		// something along these lines. This is not the case
		// at the moment. We just need to set the query argument.
		//
		// route := mux.CurrentRoute(r)
		// path, err := route.GetPathTemplate()
		// if err != nil {
		// 	// I'd like to panic, but I don' want to kill a full
		// 	// peer just because of a buggy use.
		// 	logger.Critical("BUG: wrong use of slashHandler")
		// 	origHandler(w, r) // proceed as nothing
		// 	return
		// }
		// fixedPath := strings.TrimSuffix(path, "/{arg}")
		// r.URL.Path = url.PathEscape(fixedPath)
		// r.URL.RawPath = fixedPath

		q := r.URL.Query()
		q.Set("arg", arg)
		r.URL.RawQuery = q.Encode()
		origHandler(w, r)
	}
}

func extractUID(u *url.URL) (string, bool) {
	uid := u.Query().Get("uid")
	if uid != "" {
		return uid, true
	}

	p := strings.TrimPrefix(u.Path, "/api/v0/")
	segs := strings.Split(p, "/")

	if len(segs) > 2 {
		warnMsg := "You are using an undocumented form of the IPFS API."
		warnMsg += "Consider passing your command arguments"
		warnMsg += "with the '?arg=' query parameter"
		logger.Warning(warnMsg)
		return segs[len(segs)-1], true
	}
	return "", false
}

func (proxy *Server) uidSpawn(uid string) error {
	err := proxy.rpcClient.CallContext(
		proxy.ctx,
		"",
		"Cluster",
		"SyncKey",
		uid,
		&struct{}{},
	)

	return err
}

func (proxy *Server) uidNewHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	UIDSecret := api.UIDSecret{}

	randName, err := uuid.NewV4()
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}
	name := "uid-" + randName.String()

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"UidNew",
		name,
		&UIDSecret,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	res := ipfsUidNewResp{
		UID:    UIDSecret.UID,
		PeerID: UIDSecret.PeerID,
	}
	resBytes, _ := json.Marshal(res)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

func (proxy *Server) uidRenewHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	oldUID := q.Get("uid")
	if oldUID == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	randName, err := uuid.NewV4()
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}
	newUID := "uid-" + randName.String()

	UIDRenew := api.UIDRenew{}
	err = proxy.rpcClient.CallContext(
		proxy.ctx,
		"",
		"Cluster",
		"SyncUidRenew",
		[]string{oldUID, newUID},
		&UIDRenew,
	)

	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	res := ipfsUidRenewResp{
		UID:    UIDRenew.UID,
		OldUID: UIDRenew.OldUID,
		PeerID: UIDRenew.PeerID,
	}
	resBytes, _ := json.Marshal(res)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

func (proxy *Server) uidInfoHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	UIDSecret := api.UIDSecret{}
	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"UidInfo",
		uid,
		&UIDSecret,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	resBytes, _ := json.Marshal(UIDSecret)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

func (proxy *Server) uidLoginHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	hash := q.Get("hash")
	if hash == "" {
		ipfsErrorResponder(w, "Missing hash : error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"UidLogin",
		[]string{uid, hash},
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) fileGetHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	var FileGet []byte

	q := r.URL.Query()

	arg := q.Get("arg")
	if arg == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	output := q.Get("output")
	archive := q.Get("archive")
	compress := q.Get("compress")
	compressionLevel := q.Get("compression-level")

	err := proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFileGet",
		[]string{arg, output, archive, compress, compressionLevel},
		&FileGet,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(FileGet)
	return
}

func (proxy *Server) fileCatHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	var FileGet []byte

	q := r.URL.Query()

	arg := q.Get("arg")
	if arg == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	output := q.Get("output")
	archive := q.Get("archive")
	compress := q.Get("compress")
	compressionLevel := q.Get("compression-level")

	err := proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFileGet",
		[]string{arg, output, archive, compress, compressionLevel},
		&FileGet,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	// create io.Reader
	rspbuf := bytes.NewReader(FileGet)

	// create path
	fpath, err := ioutil.TempDir("", "ipfsget")
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}
	defer os.RemoveAll(fpath)

	// extract
	extractor := &tar.Extractor{Path: fpath}
	extractor.Extract(rspbuf)

	// read files from the path
	files, err := ioutil.ReadDir(fpath)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	for _, file := range files {
		buf, err := ioutil.ReadFile(fpath + "/" + file.Name())
		if err != nil {
			ipfsErrorResponder(w, err.Error())
			return
		}
		w.Write(buf)
	}

	return
}

func (proxy *Server) filesCpHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	source := q.Get("source")
	if source == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	dest := q.Get("dest")
	if dest == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesCp",
		[]string{uid, source, dest},
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) filesFlushHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		path = "/"
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesFlush",
		[]string{uid, path},
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) filesLsHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	FilesLs := api.FilesLs{}

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		path = "/"
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesLs",
		[]string{uid, path},
		&FilesLs,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	resBytes, _ := json.Marshal(FilesLs)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

func (proxy *Server) filesMkdirHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		path = "/"
	}

	parents := q.Get("parents")
	if parents == "" {
		parents = "false"
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesMkdir",
		[]string{uid, path, parents},
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) filesMvHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	source := q.Get("source")
	if source == "" {
		source = "/"
	}

	dest := q.Get("dest")
	if dest == "" {
		dest = "/"
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesMv",
		[]string{uid, source, dest},
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) filesReadHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	var FilesReadBuf []byte

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	offset := q.Get("offset")
	count := q.Get("count")

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesRead",
		[]string{uid, path, offset, count},
		&FilesReadBuf,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(FilesReadBuf)
	return
}

func (proxy *Server) filesRmHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}
	if path == "/" {
		ipfsErrorResponder(w, "can not remove path: "+path)
		return
	}

	recursive := q.Get("recursive")
	if recursive == "" {
		recursive = "false"
	}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesRm",
		[]string{uid, path, recursive},
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) filesStatHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	FilesStat := api.FilesStat{}

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	format := q.Get("format")
	hash := q.Get("hash")
	size := q.Get("size")
	with_local := q.Get("with-local")

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesStat",
		[]string{uid, path, format, hash, size, with_local},
		&FilesStat,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	resBytes, _ := json.Marshal(FilesStat)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}

func (proxy *Server) filesWriteHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	offset := q.Get("offset")
	create := q.Get("create")
	truncate := q.Get("truncate")
	count := q.Get("count")
	rawLeaves := q.Get("raw-leaves")
	cidVersion := q.Get("cid-version")
	hash := q.Get("hash")

	multipartReader, err := r.MultipartReader()
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	bodyBuf := &bytes.Buffer{}
	writer := multipart.NewWriter(bodyBuf)

	fileWriter, err := writer.CreateFormFile("file", "upload")
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	for {
		part, err := multipartReader.NextPart()
		if part == nil {
			break
		}

		if err != nil {
			logger.Error(err)
			ipfsErrorResponder(w, err.Error())
			return
		}

		io.Copy(fileWriter, part)
	}

	contentType := writer.FormDataContentType()
	writer.Close()

	FilesWrite := api.FilesWrite{
		ContentType: contentType,
		BodyBuf:     bodyBuf,
		Params:      []string{uid, path, offset, create, truncate, count, rawLeaves, cidVersion, hash}}

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSFilesWrite",
		FilesWrite,
		&struct{}{},
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (proxy *Server) namePublishHandler(w http.ResponseWriter, r *http.Request) {
	proxy.setHeaders(w.Header(), r)

	NamePublish := api.NamePublish{}

	q := r.URL.Query()

	uid := q.Get("uid")
	if uid == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	err := proxy.uidSpawn(uid)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	path := q.Get("path")
	if path == "" {
		ipfsErrorResponder(w, "error reading request: "+r.URL.String())
		return
	}

	lifetime := q.Get("lifetime")

	err = proxy.rpcClient.Call(
		"",
		"Cluster",
		"IPFSNamePublish",
		[]string{uid, path, lifetime},
		&NamePublish,
	)
	if err != nil {
		ipfsErrorResponder(w, err.Error())
		return
	}

	resBytes, _ := json.Marshal(NamePublish)
	w.WriteHeader(http.StatusOK)
	w.Write(resBytes)
	return
}
