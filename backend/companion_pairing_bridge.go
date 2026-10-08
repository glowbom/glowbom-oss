package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const companionDiscoveryLifetime = 5 * time.Minute
const companionPairingRequestLimit = 32

type companionNearbyRequest struct {
	ID, DeviceName, Commitment, ServerChallenge, Code, Status string
	Expires                                                   time.Time
	LastPoll                                                  time.Time
	Failures                                                  int
}

type companionNearbyPeer struct {
	Since    time.Time
	Requests int
}

type companionPairingBridge struct {
	mu                      sync.Mutex
	operationMu             sync.Mutex
	session                 *companionSession
	id                      string
	register                companionDiscoveryRegister
	registration            *companionDiscoveryRegistration
	cancel                  context.CancelFunc
	expires                 time.Time
	generation              uint64
	active                  bool
	failure                 string
	requests                map[string]*companionNearbyRequest
	peers                   map[string]companionNearbyPeer
	windowCount, totalCount int
	lan                     *net.IPNet
}

func companionDiscoveryLAN(host string) *net.IPNet {
	address, _, err := net.SplitHostPort(host)
	if err != nil {
		return nil
	}
	selected := net.ParseIP(address)
	interfaces, _ := net.Interfaces()
	for _, item := range interfaces {
		if !companionLANInterface(item.Flags) {
			continue
		}
		addresses, _ := item.Addrs()
		for _, value := range addresses {
			ip, subnet, err := net.ParseCIDR(value.String())
			if err == nil && ip.Equal(selected) {
				return subnet
			}
		}
	}
	return nil
}

func companionHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func companionPairingDeviceName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 80 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

func companionPairingRandom(bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return ""
	}
	return hex.EncodeToString(value)
}

func companionPairingRequestID() string {
	value := companionPairingRandom(16)
	if value == "" {
		return ""
	}
	return value[:8] + "-" + value[8:12] + "-4" + value[13:16] + "-a" + value[17:20] + "-" + value[20:]
}

func companionNearbyCommitment(id, fingerprint, clientChallenge string) string {
	digest := sha256.Sum256([]byte("glowbom-nearby-client-v1\n" + id + "\n" + fingerprint + "\n" + clientChallenge))
	return hex.EncodeToString(digest[:])
}

func companionNearbyCode(id, fingerprint, clientChallenge, serverChallenge string) string {
	digest := sha256.Sum256([]byte("glowbom-nearby-v1\n" + id + "\n" + fingerprint + "\n" + clientChallenge + "\n" + serverChallenge))
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(digest[:8])%1000000)
}

func newCompanionPairingBridge(session *companionSession, register companionDiscoveryRegister) *companionPairingBridge {
	return &companionPairingBridge{session: session, id: companionPairingRandom(16), register: register,
		requests: map[string]*companionNearbyRequest{}, peers: map[string]companionNearbyPeer{}, lan: companionDiscoveryLAN(session.host)}
}

func (b *companionPairingBridge) cancelLocked(message string) {
	b.active, b.failure = false, message
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	if b.registration != nil {
		b.registration.Close()
		b.registration = nil
	}
	for _, request := range b.requests {
		if request.Status == "confirming" || request.Status == "waiting" || request.Status == "approved" {
			request.Status = "canceled"
		}
	}
}

func (b *companionPairingBridge) stop(message string) {
	b.operationMu.Lock()
	defer b.operationMu.Unlock()
	b.mu.Lock()
	b.generation++
	b.cancelLocked(message)
	b.mu.Unlock()
}

func (b *companionPairingBridge) start() {
	b.operationMu.Lock()
	defer b.operationMu.Unlock()
	b.mu.Lock()
	b.cancelLocked("")
	b.generation++
	generation := b.generation
	now := b.session.now()
	if b.session.ctx.Err() != nil || !now.Before(b.session.expires) || b.register == nil || b.id == "" || b.lan == nil {
		b.mu.Unlock()
		return
	}
	b.expires = now.Add(companionDiscoveryLifetime)
	if b.expires.After(b.session.expires) {
		b.expires = b.session.expires
	}
	b.windowCount = 0
	b.pruneLocked(now)
	ctx, cancel := context.WithCancel(b.session.ctx)
	b.cancel = cancel
	advertisement := companionDiscoveryAdvertisement{ID: b.id, Name: b.session.pairing.Name, Host: b.session.host, CertificateSHA256: b.session.pairing.CertificateSHA256}
	b.mu.Unlock()
	registration, err := b.register(ctx, advertisement)
	b.mu.Lock()
	if err != nil || registration == nil || registration.Close == nil || registration.Done == nil {
		cancel()
		if registration != nil && registration.Close != nil {
			registration.Close()
		}
		b.cancel = nil
		b.failure = "Nearby discovery is unavailable. Use the QR code or copied connection code."
		b.mu.Unlock()
		return
	}
	b.registration, b.active = registration, true
	remaining := b.expires.Sub(now)
	b.mu.Unlock()
	go func() {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		message := ""
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-registration.Done:
			if ctx.Err() == nil {
				message = "Nearby discovery stopped. Try again or use the copied connection code."
			}
		}
		b.mu.Lock()
		if b.generation == generation {
			b.cancelLocked(message)
		}
		b.mu.Unlock()
	}()
}

func (b *companionPairingBridge) pruneLocked(now time.Time) {
	for id, request := range b.requests {
		if !now.Before(request.Expires) {
			request.Status = "expired"
			if now.Sub(request.Expires) > companionDiscoveryLifetime {
				delete(b.requests, id)
			}
		}
	}
	for peer, limit := range b.peers {
		if now.Sub(limit.Since) >= time.Minute {
			delete(b.peers, peer)
		}
	}
}

func (b *companionPairingBridge) snapshot() (map[string]any, []map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.session.now()
	b.pruneLocked(now)
	if b.active && (!now.Before(b.expires) || b.session.ctx.Err() != nil) {
		b.cancelLocked("")
	}
	discovery := map[string]any{"active": b.active}
	if b.active {
		discovery["expiresAt"] = b.expires.UTC().Format(time.RFC3339)
	}
	if b.failure != "" {
		discovery["message"] = b.failure
	}
	pending := []map[string]string{}
	for _, request := range b.requests {
		if request.Status == "waiting" {
			pending = append(pending, map[string]string{"id": request.ID, "deviceName": request.DeviceName, "code": request.Code, "expiresAt": request.Expires.UTC().Format(time.RFC3339)})
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i]["id"] < pending[j]["id"] })
	return discovery, pending
}

func (b *companionPairingBridge) requestSnapshot(request *companionNearbyRequest) map[string]any {
	response := map[string]any{"requestID": request.ID, "status": request.Status, "serverChallenge": request.ServerChallenge, "expiresAt": request.Expires.UTC().Format(time.RFC3339)}
	if request.Code != "" {
		response["code"] = request.Code
	}
	if request.Status == "approved" && b.active && b.session.ctx.Err() == nil && b.session.now().Before(request.Expires) {
		response["pairing"] = b.session.pairing
	}
	return response
}

func (b *companionPairingBridge) serve(w http.ResponseWriter, r *http.Request) {
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	if b.lan == nil || !b.lan.Contains(net.ParseIP(peer)) {
		http.Error(w, "Use the same local network as Desktop.", http.StatusForbidden)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer func() {
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	}()
	path := strings.TrimPrefix(r.URL.Path, companionPrefix+"/pairing/")
	if path == "requests" && r.Method == http.MethodPost {
		b.create(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "requests" || len(parts[1]) != 36 {
		http.NotFound(w, r)
		return
	}
	if !(len(parts) == 2 && r.Method == http.MethodDelete) && !(len(parts) == 3 && r.Method == http.MethodPost && (parts[2] == "confirm" || parts[2] == "status")) {
		http.NotFound(w, r)
		return
	}
	var input struct {
		ClientChallenge string `json:"clientChallenge"`
	}
	if !companionDecode(w, r, &input, 1024) {
		return
	}
	if !companionHex(input.ClientChallenge, 64) {
		http.Error(w, "Invalid pairing challenge.", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.session.now()
	b.pruneLocked(now)
	request := b.requests[parts[1]]
	if request == nil {
		http.NotFound(w, r)
		return
	}
	commitment := companionNearbyCommitment(b.id, b.session.pairing.CertificateSHA256, input.ClientChallenge)
	if subtle.ConstantTimeCompare([]byte(commitment), []byte(request.Commitment)) != 1 {
		request.Failures++
		if request.Failures >= 5 && (request.Status == "confirming" || request.Status == "waiting") {
			request.Status = "canceled"
		}
		http.Error(w, "This pairing request is not yours.", http.StatusForbidden)
		return
	}
	if request.Code == "" {
		request.Code = companionNearbyCode(b.id, b.session.pairing.CertificateSHA256, input.ClientChallenge, request.ServerChallenge)
	}
	if r.Method == http.MethodDelete {
		request.Status = "canceled"
		writeJSON(w, b.requestSnapshot(request))
		return
	}
	if !b.active || !now.Before(b.expires) || b.session.ctx.Err() != nil {
		if request.Status == "confirming" || request.Status == "waiting" || request.Status == "approved" {
			request.Status = "canceled"
		}
	}
	if parts[2] == "confirm" && request.Status == "confirming" {
		request.Status = "waiting"
	}
	if !request.LastPoll.IsZero() && now.Sub(request.LastPoll) < 250*time.Millisecond {
		http.Error(w, "Wait before checking again.", http.StatusTooManyRequests)
		return
	}
	request.LastPoll = now
	writeJSON(w, b.requestSnapshot(request))
}

func (b *companionPairingBridge) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version           int    `json:"version"`
		DesktopID         string `json:"desktopID"`
		CertificateSHA256 string `json:"certificateSHA256"`
		ClientCommitment  string `json:"clientCommitment"`
		DeviceName        string `json:"deviceName"`
	}
	if !companionDecode(w, r, &input, 2048) {
		return
	}
	if input.Version != 1 || input.DesktopID != b.id || input.CertificateSHA256 != b.session.pairing.CertificateSHA256 || !companionHex(input.ClientCommitment, 64) || companionPairingDeviceName(input.DeviceName) == "" {
		http.Error(w, "This Desktop pairing identity does not match.", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.session.now()
	b.pruneLocked(now)
	if !b.active || !now.Before(b.expires) || b.session.ctx.Err() != nil {
		http.Error(w, "Open nearby pairing on Desktop again.", http.StatusGone)
		return
	}
	for _, request := range b.requests {
		if request.Commitment == input.ClientCommitment && now.Before(request.Expires) {
			writeJSON(w, b.initialSnapshot(request))
			return
		}
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	rate := b.peers[peer]
	if b.windowCount >= companionPairingRequestLimit || b.totalCount >= 1000 || len(b.requests) >= 64 || rate.Requests >= 4 || (rate.Requests == 0 && len(b.peers) >= 64) {
		http.Error(w, "Too many pairing requests. Wait and try again on Desktop.", http.StatusTooManyRequests)
		return
	}
	if rate.Requests == 0 {
		rate.Since = now
	}
	rate.Requests++
	b.peers[peer] = rate
	request := &companionNearbyRequest{ID: companionPairingRequestID(), DeviceName: strings.TrimSpace(input.DeviceName), Commitment: input.ClientCommitment, ServerChallenge: companionPairingRandom(32), Status: "confirming", Expires: b.expires}
	if request.ID == "" || request.ServerChallenge == "" {
		http.Error(w, "Could not start nearby pairing.", http.StatusServiceUnavailable)
		return
	}
	b.requests[request.ID] = request
	b.windowCount++
	b.totalCount++
	writeJSON(w, b.initialSnapshot(request))
}

func (b *companionPairingBridge) initialSnapshot(request *companionNearbyRequest) map[string]any {
	return map[string]any{"requestID": request.ID, "status": "confirming", "serverChallenge": request.ServerChallenge, "expiresAt": request.Expires.UTC().Format(time.RFC3339)}
}

func (m *companionManager) pairingControl(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session == nil || session.ctx.Err() != nil || !m.now().Before(session.expires) || session.pairingBridge == nil {
		http.Error(w, "Enable the mobile connection first.", http.StatusConflict)
		return
	}
	b := session.pairingBridge
	if r.URL.Path == "/companion/pairing/discovery" {
		if r.Method == http.MethodPost {
			b.start()
		} else if r.Method == http.MethodDelete {
			b.stop("")
		} else {
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, m.status())
		return
	}
	if r.URL.Path != "/companion/pairing/respond" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var input struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
		Code     string `json:"code"`
	}
	if !companionDecode(w, r, &input, 1024) {
		return
	}
	if err := b.respond(input.ID, input.Decision, input.Code); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, m.status())
}

func (b *companionPairingBridge) respond(id, decision, code string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.session.now()
	b.pruneLocked(now)
	request := b.requests[id]
	if !b.active || !now.Before(b.expires) || b.session.ctx.Err() != nil || request == nil || !now.Before(request.Expires) || request.Code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(request.Code)) != 1 {
		return errors.New("This nearby pairing request expired or changed. Check both devices again.")
	}
	status := ""
	if decision == "approve" {
		status = "approved"
	} else if decision == "reject" {
		status = "rejected"
	} else {
		return errors.New("Choose approve or reject.")
	}
	if request.Status == status {
		return nil
	}
	if request.Status != "waiting" {
		return errors.New("This nearby pairing request is no longer waiting.")
	}
	request.Status = status
	return nil
}
