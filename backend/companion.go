package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const companionLifetime = 8 * time.Hour
const companionPrefix = "/companion/v1"

type companionInterface struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

type companionPairing struct {
	Version           int    `json:"version"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	Token             string `json:"token"`
	CertificateSHA256 string `json:"certificateSHA256"`
	ExpiresAt         string `json:"expiresAt"`
}

type companionProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"createdAt,omitempty"`
	AssetCount int    `json:"assetCount"`
	Available  bool   `json:"available"`
	path       string
}

type companionManager struct {
	mu                sync.Mutex
	buildMu           sync.Mutex
	building          map[string]bool
	session           *companionSession
	api               http.Handler
	previews          *projectPreviewManager
	interfaces        func() []companionInterface
	now               func() time.Time
	runMu             sync.Mutex
	runs              map[string]*companionJob
	runContext        context.Context
	stopRuns          context.CancelFunc
	registerDiscovery companionDiscoveryRegister
}

type companionSession struct {
	mu            sync.Mutex
	pairing       companionPairing
	host          string
	expires       time.Time
	projects      map[string]companionProject
	jobs          map[string]*companionJob
	buildModels   map[string]companionBuildModel
	buildTargets  map[string][]string
	attachments   map[string]companionAttachment
	uploadsActive int
	uploadBytes   int64
	modelRevision uint64
	api           http.Handler
	previews      *projectPreviewManager
	now           func() time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	server        *http.Server
	timer         *time.Timer
	manager       *companionManager
	browserMu     sync.Mutex
	browsers      map[string]*companionBrowserSession
	// Tests replace the LAN listener with an isolated loopback listener.
	browserListen         func(string, string) (net.Listener, error)
	localChatPath         string
	transfersActive       int
	transferCount         int
	transferredBytes      int64
	transferReservedBytes int64
	importDestinations    map[string]companionImportDestination
	destinationRequests   map[string]string
	pickImportFolder      func(context.Context) (string, bool, error)
	importTemplateCLI     accountCLIRunner
	prototypeJobs         map[string]*companionDesktopPrototypeJob
	iconJobs              map[string]*companionProjectIconJob
	pairingBridge         *companionPairingBridge
}

func newCompanionManager(api http.Handler) *companionManager {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &companionManager{api: api, interfaces: companionInterfaces, now: time.Now, building: map[string]bool{}, runs: map[string]*companionJob{}, runContext: ctx, stopRuns: cancel, registerDiscovery: registerCompanionDiscovery}
	return manager
}

func companionPrivateIPv4(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback()
}

func companionLANInterface(flags net.Flags) bool {
	return flags&net.FlagUp != 0 && flags&(net.FlagLoopback|net.FlagPointToPoint) == 0
}

func companionInterfaces() []companionInterface {
	result := []companionInterface{}
	interfaces, _ := net.Interfaces()
	for _, item := range interfaces {
		if !companionLANInterface(item.Flags) {
			continue
		}
		addresses, _ := item.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && companionPrivateIPv4(ip) {
				result = append(result, companionInterface{Address: ip.String(), Name: item.Name})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Address < result[j].Address })
	return result
}

func companionCertificate(address string, now, expires time.Time) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Glowbom Desktop companion"},
		NotBefore: now.Add(-time.Minute), NotAfter: expires,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(address)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	digest := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(digest[:]), nil
}

func companionProjectID(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:16])
}

func companionProjects(paths []string) (map[string]companionProject, error) {
	if len(paths) > 40 {
		return nil, errors.New("Choose up to 40 recent projects.")
	}
	result := map[string]companionProject{}
	// Studio already records folders opened through its project controls.
	if saved, err := listRegisteredStudioProjects(); err == nil {
		for _, project := range saved {
			if project.Available {
				paths = append(paths, project.Path)
			}
		}
	}
	for _, path := range paths {
		root, err := chatProjectRoot(path)
		if err != nil {
			continue
		}
		project, err := LoadProject(GetProjectPaths(root).Manifest)
		if err != nil {
			continue
		}
		id := companionProjectID(root)
		result[id] = companionProject{ID: id, Name: project.Name, CreatedAt: companionProjectDate(project.CreatedAt), Available: true, path: root}
	}
	return result, nil
}

func companionDecode(w http.ResponseWriter, r *http.Request, value any, limit int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "Invalid companion request.", http.StatusBadRequest)
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "Send one companion request.", http.StatusBadRequest)
		return false
	}
	return true
}

func (m *companionManager) localAllowed(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || !isLoopbackHost(backendBindHost()) {
		http.Error(w, "Use these controls on Desktop.", http.StatusForbidden)
		return false
	}
	if token := glowbomServerToken(); token == "" || !hasValidGlowbomServerToken(r, token) {
		http.Error(w, "Desktop authentication is required.", http.StatusUnauthorized)
		return false
	}
	if !isAllowedOrigin(r, glowbomAllowedOrigins()) {
		http.Error(w, "Origin not allowed.", http.StatusForbidden)
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func (m *companionManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !m.localAllowed(w, r) {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/companion/pairing/") {
		m.pairingControl(w, r)
		return
	}
	if r.URL.Path == "/companion/build-model" {
		m.buildModel(w, r)
		return
	}
	if r.URL.Path == "/companion/cancel" {
		m.cancelJob(w, r)
		return
	}
	if r.URL.Path == "/companion/responded" {
		m.responded(w, r)
		return
	}
	if r.URL.Path == "/companion/respond" {
		m.respond(w, r)
		return
	}
	if r.URL.Path != "/companion" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, m.status())
	case http.MethodDelete:
		m.Close()
		writeJSON(w, m.status())
	case http.MethodPost:
		var request struct {
			Address      string   `json:"address"`
			ProjectPaths []string `json:"projectPaths"`
		}
		if !companionDecode(w, r, &request, 32<<10) {
			return
		}
		valid := false
		for _, candidate := range m.interfaces() {
			valid = valid || request.Address == candidate.Address
		}
		if !valid || !companionPrivateIPv4(net.ParseIP(request.Address)) {
			http.Error(w, "Choose this computer's private network address.", http.StatusBadRequest)
			return
		}
		projects, err := companionProjects(request.ProjectPaths)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := m.enable(request.Address, projects); err != nil {
			http.Error(w, "Could not open the companion connection. Check the selected network.", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, m.status())
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
	}
}

func (m *companionManager) enable(address string, projects map[string]companionProject) error {
	for _, project := range projects {
		m.loadProjectRuns(project.path)
	}
	now := m.now()
	expires := now.Add(companionLifetime)
	certificate, fingerprint, err := companionCertificate(address, now, expires)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, "0"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	name, _ := os.Hostname()
	name = companionPairingDeviceName(name)
	if name == "" {
		name = "Glowbom Desktop"
	}
	session := &companionSession{
		host: listener.Addr().String(), expires: expires, projects: projects, jobs: map[string]*companionJob{},
		api: m.api, previews: m.previews, now: m.now, ctx: ctx, cancel: cancel, manager: m,
	}
	session.pairing = companionPairing{Version: 1, Name: name, URL: "https://" + session.host + companionPrefix,
		Token: hex.EncodeToString(secret), CertificateSHA256: fingerprint, ExpiresAt: expires.UTC().Format(time.RFC3339)}
	session.pairingBridge = newCompanionPairingBridge(session, m.registerDiscovery)
	session.server = &http.Server{Handler: session, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 8 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}}
	session.timer = time.AfterFunc(companionLifetime, session.close)
	m.mu.Lock()
	previous := m.session
	m.session = session
	m.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	go func() {
		_ = session.server.Serve(tls.NewListener(listener, session.server.TLSConfig))
		session.cancel()
	}()
	session.pairingBridge.start()
	return nil
}

func (s *companionSession) close() {
	s.cancel()
	if s.pairingBridge != nil {
		s.pairingBridge.stop("")
	}
	s.closeBrowserSessions()
	s.mu.Lock()
	localChatPath := s.localChatPath
	s.localChatPath = ""
	s.mu.Unlock()
	if localChatPath != "" {
		_ = os.RemoveAll(localChatPath)
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.server != nil {
		_ = s.server.Close()
	}
}

func (m *companionManager) Close() {
	m.mu.Lock()
	session := m.session
	m.session = nil
	m.mu.Unlock()
	if session != nil {
		session.close()
	}
}

func (m *companionManager) status() map[string]any {
	status := map[string]any{"active": false, "interfaces": m.interfaces(), "jobs": m.runSnapshots(true)}
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session == nil || session.ctx.Err() != nil || !m.now().Before(session.expires) {
		return status
	}
	status["active"], status["pairing"] = true, session.pairing
	status["jobs"] = m.runSnapshots(true)
	status["buildModels"] = session.buildModelList(true)
	if session.pairingBridge != nil {
		status["discovery"], status["pairingRequests"] = session.pairingBridge.snapshot()
	}
	return status
}

func (m *companionManager) responded(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		JobID string `json:"jobId"`
		Kind  string `json:"kind"`
		ID    string `json:"id"`
	}
	if !companionDecode(w, r, &request, 1024) {
		return
	}
	job := m.run(request.JobID)
	if job == nil || !job.acknowledge(request.Kind, request.ID) {
		http.Error(w, "This request is no longer waiting.", http.StatusConflict)
		return
	}
	writeJSON(w, m.status())
}

func (s *companionSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.ctx.Err() != nil || !s.now().Before(s.expires) {
		http.Error(w, "This connection expired. Pair again on Desktop.", http.StatusUnauthorized)
		return
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	contentRequest := companionPreviewContentRoute(r.URL.Path)
	if err != nil || !companionPrivateIPv4(net.ParseIP(remote)) || r.Host != s.host || r.Header.Get("Origin") != "" || r.URL.User != nil ||
		(!contentRequest && ((r.URL.RawQuery != "" && !companionAudioPageRequest(r)) || r.URL.EscapedPath() != r.URL.Path)) ||
		(contentRequest && (len(r.URL.RawQuery) > 4096 || strings.ContainsAny(r.URL.Path, "\x00\\%"))) {
		http.Error(w, "This request is not allowed.", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, companionPrefix+"/pairing/") {
		if r.TLS == nil || s.pairingBridge == nil {
			http.NotFound(w, r)
			return
		}
		s.pairingBridge.serve(w, r)
		return
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.pairing.Token)) != 1 {
		http.Error(w, "Pair with Desktop to continue.", http.StatusUnauthorized)
		return
	}
	s.route(w, r)
}
