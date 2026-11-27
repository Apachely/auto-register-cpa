package registerbot

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
)

const proxyPoolDocumentVersion = 1

// ProxyPoolError exposes a fixed operational reason without leaking controller
// responses or proxy details into the queue journal.
type ProxyPoolError struct{ Reason string }

func (e *ProxyPoolError) Error() string { return "registerbot: " + e.Reason }

func proxyPoolReason(err error) string {
	var failure *ProxyPoolError
	if errors.As(err, &failure) {
		switch failure.Reason {
		case "proxy_pool_no_nodes", "proxy_pool_unavailable", "proxy_pool_rotation_exhausted", "proxy_pool_quota_exhausted", "proxy_pool_state_failed":
			return failure.Reason
		}
	}
	return ""
}

type proxyPoolGateway interface {
	Probe(context.Context, bool) Status
	NodePool(ChannelSettings) []string
	Acquire(context.Context, ChannelSettings, string) (PinResult, error)
	Release(string, string)
}

type proxyPoolNode struct {
	Group         string `json:"group"`
	Node          string `json:"node"`
	ExitIP        string `json:"exit_ip,omitempty"`
	Active        bool   `json:"active"`
	Assignments   int    `json:"assignments"`
	Failures      int    `json:"failures"`
	LastUsedAt    int64  `json:"last_used_at,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastReason    string `json:"last_reason,omitempty"`
}

type proxyPoolIP struct {
	ExitIP           string `json:"exit_ip"`
	UsableAccounts   int    `json:"usable_accounts"`
	UnusableAccounts int    `json:"unusable_accounts"`
	LastUsedAt       int64  `json:"last_used_at,omitempty"`
	LastSuccessAt    int64  `json:"last_success_at,omitempty"`
}

type proxyPoolReservation struct {
	Holder     string `json:"holder"`
	Group      string `json:"group"`
	Node       string `json:"node"`
	ExitIP     string `json:"exit_ip"`
	ReservedAt int64  `json:"reserved_at"`
}

type proxyPoolCompletion struct {
	ExitIP      string `json:"exit_ip,omitempty"`
	Usable      bool   `json:"usable"`
	CompletedAt int64  `json:"completed_at"`
}

type proxyPoolDocument struct {
	Version      int                             `json:"version"`
	Cursor       int                             `json:"cursor"`
	LastNode     string                          `json:"last_node,omitempty"`
	LastExitIP   string                          `json:"last_exit_ip,omitempty"`
	UpdatedAt    int64                           `json:"updated_at"`
	Nodes        map[string]proxyPoolNode        `json:"nodes"`
	IPs          map[string]proxyPoolIP          `json:"ips"`
	Reservations map[string]proxyPoolReservation `json:"reservations"`
	Completed    map[string]proxyPoolCompletion  `json:"completed"`
}

// ProxyPoolNodeView is one gateway node and its most recently observed exit.
type ProxyPoolNodeView struct {
	Group          string `json:"group"`
	Node           string `json:"node"`
	ExitIP         string `json:"exit_ip,omitempty"`
	Status         string `json:"status"`
	UsableAccounts int    `json:"usable_accounts"`
	Reserved       int    `json:"reserved"`
	Assignments    int    `json:"assignments"`
	Failures       int    `json:"failures"`
	LastUsedAt     int64  `json:"last_used_at,omitempty"`
}

// ProxyPoolIPView is the durable per-exit quota ledger exposed to management.
type ProxyPoolIPView struct {
	ExitIP           string `json:"exit_ip"`
	UsableAccounts   int    `json:"usable_accounts"`
	UnusableAccounts int    `json:"unusable_accounts"`
	Reserved         int    `json:"reserved"`
	Remaining        int    `json:"remaining"`
	LastUsedAt       int64  `json:"last_used_at,omitempty"`
	LastSuccessAt    int64  `json:"last_success_at,omitempty"`
}

// ProxyPoolSnapshot is a sanitized management view of the scheduler and quota ledger.
type ProxyPoolSnapshot struct {
	Enabled                bool                `json:"enabled"`
	Mode                   string              `json:"mode"`
	MaxUsableAccountsPerIP int                 `json:"max_usable_accounts_per_ip"`
	FailureCooldownSeconds int                 `json:"failure_cooldown_seconds"`
	TotalNodes             int                 `json:"total_nodes"`
	ReadyNodes             int                 `json:"ready_nodes"`
	Reserved               int                 `json:"reserved"`
	ExhaustedIPs           int                 `json:"exhausted_ips"`
	UsableAccounts         int                 `json:"usable_accounts"`
	LastNode               string              `json:"last_node,omitempty"`
	LastExitIP             string              `json:"last_exit_ip,omitempty"`
	UpdatedAt              int64               `json:"updated_at"`
	Nodes                  []ProxyPoolNodeView `json:"nodes"`
	IPs                    []ProxyPoolIPView   `json:"ips"`
}

// ProxyPool schedules nodes through the existing gateway and persists only
// routing metadata. Browser and API traffic never bypass the gateway.
type ProxyPool struct {
	mu       sync.Mutex
	selectMu sync.Mutex
	path     string
	gateway  proxyPoolGateway
	settings ProxyPoolSettings
	doc      proxyPoolDocument
}

func newProxyPool(stateDir string, settings ProxyPoolSettings, gateway proxyPoolGateway) (*ProxyPool, error) {
	pool := &ProxyPool{
		path:     filepath.Join(stateDir, "proxy-pool.json"),
		gateway:  gateway,
		settings: settings,
		doc: proxyPoolDocument{
			Version:      proxyPoolDocumentVersion,
			Nodes:        make(map[string]proxyPoolNode),
			IPs:          make(map[string]proxyPoolIP),
			Reservations: make(map[string]proxyPoolReservation),
			Completed:    make(map[string]proxyPoolCompletion),
		},
	}
	data, errRead := os.ReadFile(pool.path)
	if errors.Is(errRead, os.ErrNotExist) {
		return pool, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("registerbot: read proxy pool state: %w", errRead)
	}
	if errDecode := json.Unmarshal(data, &pool.doc); errDecode != nil {
		return nil, fmt.Errorf("registerbot: parse proxy pool state: %w", errDecode)
	}
	if pool.doc.Version != proxyPoolDocumentVersion {
		return nil, errors.New("registerbot: unsupported proxy pool state version")
	}
	pool.ensureMapsLocked()
	return pool, nil
}

func (p *ProxyPool) ensureMapsLocked() {
	if p.doc.Nodes == nil {
		p.doc.Nodes = make(map[string]proxyPoolNode)
	}
	if p.doc.IPs == nil {
		p.doc.IPs = make(map[string]proxyPoolIP)
	}
	if p.doc.Reservations == nil {
		p.doc.Reservations = make(map[string]proxyPoolReservation)
	}
	if p.doc.Completed == nil {
		p.doc.Completed = make(map[string]proxyPoolCompletion)
	}
}

func (p *ProxyPool) Configure(settings ProxyPoolSettings) {
	p.mu.Lock()
	p.settings = settings
	p.mu.Unlock()
}

// ApplyHealth replaces transient failures with the startup health snapshot for
// the dedicated proxy instance. Durable quota and completion records remain.
func (p *ProxyPool) ApplyHealth(verified map[string]string) error {
	if len(verified) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for key, record := range p.doc.Nodes {
		exitIP, exists := verified[record.Node]
		if !exists {
			continue
		}
		normalized, valid := normalizeExitIP(exitIP)
		if !valid {
			continue
		}
		if record.ExitIP != normalized || record.Failures != 0 || record.CooldownUntil != 0 || record.LastReason != "" || !record.Active {
			record.ExitIP = normalized
			record.Failures = 0
			record.CooldownUntil = 0
			record.LastReason = ""
			record.Active = true
			p.doc.Nodes[key] = record
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return p.persistLocked()
}

func (p *ProxyPool) Enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings.Enabled()
}

func proxyNodeKey(group, node string) string { return group + "\x00" + node }

func normalizeExitIP(value string) (string, bool) {
	value = strings.TrimSpace(strings.Split(value, ",")[0])
	address, errParse := netip.ParseAddr(value)
	if errParse != nil {
		return "", false
	}
	return address.Unmap().String(), true
}

func uniqueNodes(nodes []string) []string {
	seen := make(map[string]struct{}, len(nodes))
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}
		if _, exists := seen[node]; exists {
			continue
		}
		seen[node] = struct{}{}
		out = append(out, node)
	}
	return out
}

func (p *ProxyPool) syncChannelLocked(channel ChannelSettings, nodes []string) {
	for _, node := range uniqueNodes(nodes) {
		key := proxyNodeKey(channel.Group, node)
		record := p.doc.Nodes[key]
		record.Group, record.Node, record.Active = channel.Group, node, true
		p.doc.Nodes[key] = record
	}
}

func (p *ProxyPool) persistLocked() error {
	p.doc.UpdatedAt = time.Now().UnixMilli()
	data, errEncode := json.Marshal(&p.doc)
	if errEncode != nil {
		return fmt.Errorf("registerbot: encode proxy pool state: %w", errEncode)
	}
	if errWrite := atomicWrite(p.path, data); errWrite != nil {
		return fmt.Errorf("registerbot: write proxy pool state: %w", errWrite)
	}
	return nil
}

func (p *ProxyPool) reservationCountsLocked() (map[string]int, map[string]int) {
	byIP := make(map[string]int)
	byNode := make(map[string]int)
	for _, reservation := range p.doc.Reservations {
		byIP[reservation.ExitIP]++
		byNode[proxyNodeKey(reservation.Group, reservation.Node)]++
	}
	return byIP, byNode
}

func (p *ProxyPool) Snapshot() ProxyPoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	settings := p.settings
	view := ProxyPoolSnapshot{
		Enabled:                settings.Enabled(),
		Mode:                   settings.Mode,
		MaxUsableAccountsPerIP: settings.MaxUsableAccountsPerIP,
		FailureCooldownSeconds: settings.FailureCooldownSeconds,
		LastNode:               p.doc.LastNode,
		LastExitIP:             p.doc.LastExitIP,
		UpdatedAt:              p.doc.UpdatedAt,
		Nodes:                  make([]ProxyPoolNodeView, 0, len(p.doc.Nodes)),
		IPs:                    make([]ProxyPoolIPView, 0, len(p.doc.IPs)),
	}
	reservedByIP, reservedByNode := p.reservationCountsLocked()
	now := time.Now().UnixMilli()
	for key, record := range p.doc.Nodes {
		if !record.Active {
			continue
		}
		status := "untested"
		usable := 0
		if record.ExitIP != "" {
			usable = p.doc.IPs[record.ExitIP].UsableAccounts
			status = "ready"
		}
		reserved := reservedByNode[key]
		switch {
		case record.ExitIP != "" && usable+reservedByIP[record.ExitIP] >= settings.MaxUsableAccountsPerIP:
			status = "exhausted"
		case record.CooldownUntil > now:
			status = "cooldown"
		case reserved > 0:
			status = "reserved"
		}
		view.Nodes = append(view.Nodes, ProxyPoolNodeView{
			Group: record.Group, Node: record.Node, ExitIP: record.ExitIP, Status: status,
			UsableAccounts: usable, Reserved: reserved, Assignments: record.Assignments,
			Failures: record.Failures, LastUsedAt: record.LastUsedAt,
		})
		view.TotalNodes++
		if status == "ready" || status == "reserved" {
			view.ReadyNodes++
		}
	}
	for exitIP, record := range p.doc.IPs {
		reserved := reservedByIP[exitIP]
		remaining := settings.MaxUsableAccountsPerIP - record.UsableAccounts - reserved
		if remaining < 0 {
			remaining = 0
		}
		if record.UsableAccounts >= settings.MaxUsableAccountsPerIP {
			view.ExhaustedIPs++
		}
		view.UsableAccounts += record.UsableAccounts
		view.Reserved += reserved
		view.IPs = append(view.IPs, ProxyPoolIPView{
			ExitIP: exitIP, UsableAccounts: record.UsableAccounts,
			UnusableAccounts: record.UnusableAccounts, Reserved: reserved, Remaining: remaining,
			LastUsedAt: record.LastUsedAt, LastSuccessAt: record.LastSuccessAt,
		})
	}
	sort.Slice(view.Nodes, func(i, j int) bool {
		if view.Nodes[i].Group == view.Nodes[j].Group {
			return view.Nodes[i].Node < view.Nodes[j].Node
		}
		return view.Nodes[i].Group < view.Nodes[j].Group
	})
	sort.Slice(view.IPs, func(i, j int) bool {
		if view.IPs[i].UsableAccounts == view.IPs[j].UsableAccounts {
			return view.IPs[i].ExitIP < view.IPs[j].ExitIP
		}
		return view.IPs[i].UsableAccounts > view.IPs[j].UsableAccounts
	})
	return view
}

func (p *ProxyPool) Refresh(ctx context.Context, channels []ChannelSettings) (ProxyPoolSnapshot, error) {
	status := p.gateway.Probe(ctx, true)
	if status.Mode != GatewayModeController {
		return p.Snapshot(), &ProxyPoolError{Reason: "proxy_pool_unavailable"}
	}
	p.mu.Lock()
	previousNodes := make(map[string]proxyPoolNode, len(p.doc.Nodes))
	for key, record := range p.doc.Nodes {
		previousNodes[key] = record
	}
	previousUpdatedAt := p.doc.UpdatedAt
	for key, record := range p.doc.Nodes {
		record.Active = false
		p.doc.Nodes[key] = record
	}
	for _, channel := range channels {
		if !channel.Enabled || channel.ManualMode {
			continue
		}
		p.syncChannelLocked(channel, p.gateway.NodePool(channel))
	}
	errSave := p.persistLocked()
	if errSave != nil {
		p.doc.Nodes = previousNodes
		p.doc.UpdatedAt = previousUpdatedAt
	}
	p.mu.Unlock()
	if errSave != nil {
		return p.Snapshot(), &ProxyPoolError{Reason: "proxy_pool_state_failed"}
	}
	return p.Snapshot(), nil
}

func rotateCandidates(nodes []string, cursor int) []string {
	if len(nodes) < 2 {
		return append([]string(nil), nodes...)
	}
	start := cursor % len(nodes)
	return append(append([]string(nil), nodes[start:]...), nodes[:start]...)
}

func (p *ProxyPool) markFailure(group, node, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := proxyNodeKey(group, node)
	record, existed := p.doc.Nodes[key]
	previous, previousUpdatedAt := record, p.doc.UpdatedAt
	record.Group, record.Node, record.Active = group, node, true
	record.Failures++
	record.LastReason = reason
	record.CooldownUntil = time.Now().Add(time.Duration(p.settings.FailureCooldownSeconds) * time.Second).UnixMilli()
	p.doc.Nodes[key] = record
	if errSave := p.persistLocked(); errSave != nil {
		if existed {
			p.doc.Nodes[key] = previous
		} else {
			delete(p.doc.Nodes, key)
		}
		p.doc.UpdatedAt = previousUpdatedAt
		return errSave
	}
	return nil
}

func (p *ProxyPool) reservedSlotsLocked(exitIP, holder string) int {
	count := 0
	for rowID, reservation := range p.doc.Reservations {
		if rowID != holder && reservation.ExitIP == exitIP {
			count++
		}
	}
	return count
}

// Acquire reserves one verified, non-exhausted exit while the existing gateway
// retains ownership of the actual forwarding lease.
func (p *ProxyPool) Acquire(ctx context.Context, channel ChannelSettings, holder string) (PinResult, error) {
	p.selectMu.Lock()
	defer p.selectMu.Unlock()
	if strings.TrimSpace(holder) == "" {
		return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
	}
	nodes := uniqueNodes(p.gateway.NodePool(channel))
	if len(nodes) == 0 {
		return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_no_nodes"}
	}
	p.mu.Lock()
	p.syncChannelLocked(channel, nodes)
	settings := p.settings
	reservation, resuming := p.doc.Reservations[holder]
	cursor := p.doc.Cursor
	p.mu.Unlock()
	if !settings.Enabled() && !resuming {
		return p.gateway.Acquire(ctx, channel, holder)
	}
	candidates := rotateCandidates(nodes, cursor)
	if resuming {
		candidates = []string{reservation.Node}
	}
	sawQuota, sawRotation, attempted := false, false, false
	for _, node := range candidates {
		p.mu.Lock()
		record := p.doc.Nodes[proxyNodeKey(channel.Group, node)]
		cooling := !resuming && record.CooldownUntil > time.Now().UnixMilli()
		p.mu.Unlock()
		if cooling {
			continue
		}
		attempted = true
		forced := channel
		forced.Nodes = []string{node}
		forced.RotateNode = false
		pin, errAcquire := p.gateway.Acquire(ctx, forced, holder)
		if errAcquire != nil {
			if ctx.Err() != nil {
				return PinResult{}, ctx.Err()
			}
			if errSave := p.markFailure(channel.Group, node, "acquire_failed"); errSave != nil {
				return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
			}
			continue
		}
		exitIP, validIP := normalizeExitIP(pin.ExitIP)
		if pin.Mode != GatewayModeController || pin.Node == "" || !pin.Verified || !validIP {
			p.gateway.Release(channel.Group, holder)
			if errSave := p.markFailure(channel.Group, node, "exit_unverified"); errSave != nil {
				return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
			}
			continue
		}
		pin.ExitIP = exitIP
		p.mu.Lock()
		ipRecord := p.doc.IPs[exitIP]
		reserved := p.reservedSlotsLocked(exitIP, holder)
		quotaFull := ipRecord.UsableAccounts+reserved >= settings.MaxUsableAccountsPerIP
		rotated := resuming || (node != p.doc.LastNode && exitIP != p.doc.LastExitIP)
		if quotaFull || !rotated {
			p.mu.Unlock()
			p.gateway.Release(channel.Group, holder)
			if quotaFull {
				sawQuota = true
				if errSave := p.markFailure(channel.Group, node, "quota_exhausted"); errSave != nil {
					return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
				}
			} else {
				sawRotation = true
				if errSave := p.markFailure(channel.Group, node, "same_exit"); errSave != nil {
					return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
				}
			}
			continue
		}
		now := time.Now().UnixMilli()
		nodeKey := proxyNodeKey(channel.Group, node)
		nodeRecord, nodeExisted := p.doc.Nodes[nodeKey]
		previousNode := nodeRecord
		previousIP, ipExisted := p.doc.IPs[exitIP]
		previousReservation, reservationExisted := p.doc.Reservations[holder]
		previousLastNode, previousLastExitIP := p.doc.LastNode, p.doc.LastExitIP
		previousCursor, previousUpdatedAt := p.doc.Cursor, p.doc.UpdatedAt
		nodeRecord.Group, nodeRecord.Node, nodeRecord.ExitIP = channel.Group, node, exitIP
		nodeRecord.Active, nodeRecord.CooldownUntil, nodeRecord.LastReason = true, 0, ""
		nodeRecord.Assignments++
		nodeRecord.LastUsedAt = now
		p.doc.Nodes[nodeKey] = nodeRecord
		ipRecord.ExitIP, ipRecord.LastUsedAt = exitIP, now
		p.doc.IPs[exitIP] = ipRecord
		p.doc.Reservations[holder] = proxyPoolReservation{
			Holder: holder, Group: channel.Group, Node: node, ExitIP: exitIP, ReservedAt: now,
		}
		p.doc.LastNode, p.doc.LastExitIP = node, exitIP
		p.doc.Cursor++
		errSave := p.persistLocked()
		if errSave != nil {
			if nodeExisted {
				p.doc.Nodes[nodeKey] = previousNode
			} else {
				delete(p.doc.Nodes, nodeKey)
			}
			if ipExisted {
				p.doc.IPs[exitIP] = previousIP
			} else {
				delete(p.doc.IPs, exitIP)
			}
			if reservationExisted {
				p.doc.Reservations[holder] = previousReservation
			} else {
				delete(p.doc.Reservations, holder)
			}
			p.doc.LastNode, p.doc.LastExitIP = previousLastNode, previousLastExitIP
			p.doc.Cursor, p.doc.UpdatedAt = previousCursor, previousUpdatedAt
		}
		p.mu.Unlock()
		if errSave != nil {
			p.gateway.Release(channel.Group, holder)
			return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
		}
		return pin, nil
	}
	switch {
	case sawQuota:
		return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_quota_exhausted"}
	case sawRotation:
		return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_rotation_exhausted"}
	case attempted:
		return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_unavailable"}
	default:
		return PinResult{}, &ProxyPoolError{Reason: "proxy_pool_unavailable"}
	}
}

func (p *ProxyPool) ReleaseRoute(group, holder string) { p.gateway.Release(group, holder) }

func (p *ProxyPool) HasReservation(holder string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.doc.Reservations[holder]
	return exists
}

// Complete records quota only after the key importer has verified and committed
// the key. Missing welcome credit remains visible but never consumes usable quota.
func (p *ProxyPool) Complete(holder string, harvest Harvest) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if completed, exists := p.doc.Completed[holder]; exists {
		return completed.Usable, nil
	}
	reservation, exists := p.doc.Reservations[holder]
	if !exists {
		return false, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
	}
	exitIP := reservation.ExitIP
	if observed, valid := normalizeExitIP(harvest.ExitIP); valid {
		exitIP = observed
	}
	usable := harvest.CreditConfirmed && strings.TrimSpace(harvest.APIKey) != ""
	now := time.Now().UnixMilli()
	ipRecord, ipExisted := p.doc.IPs[exitIP]
	previousIP, previousUpdatedAt := ipRecord, p.doc.UpdatedAt
	ipRecord.ExitIP, ipRecord.LastUsedAt = exitIP, now
	if usable {
		ipRecord.UsableAccounts++
		ipRecord.LastSuccessAt = now
	} else {
		ipRecord.UnusableAccounts++
	}
	p.doc.IPs[exitIP] = ipRecord
	delete(p.doc.Reservations, holder)
	p.doc.Completed[holder] = proxyPoolCompletion{ExitIP: exitIP, Usable: usable, CompletedAt: now}
	if errSave := p.persistLocked(); errSave != nil {
		if ipExisted {
			p.doc.IPs[exitIP] = previousIP
		} else {
			delete(p.doc.IPs, exitIP)
		}
		p.doc.Reservations[holder] = reservation
		delete(p.doc.Completed, holder)
		p.doc.UpdatedAt = previousUpdatedAt
		return false, &ProxyPoolError{Reason: "proxy_pool_state_failed"}
	}
	return usable, nil
}

func (p *ProxyPool) Abandon(holder string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	reservation, exists := p.doc.Reservations[holder]
	if !exists {
		return nil
	}
	previousUpdatedAt := p.doc.UpdatedAt
	delete(p.doc.Reservations, holder)
	if errSave := p.persistLocked(); errSave != nil {
		p.doc.Reservations[holder] = reservation
		p.doc.UpdatedAt = previousUpdatedAt
		return errSave
	}
	return nil
}

// FailReservation removes a failed account reservation and immediately cools
// down its route so the next account cannot select the same broken node.
func (p *ProxyPool) FailReservation(holder, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	reservation, exists := p.doc.Reservations[holder]
	if !exists {
		return nil
	}
	key := proxyNodeKey(reservation.Group, reservation.Node)
	record, recordExisted := p.doc.Nodes[key]
	previousRecord, previousUpdatedAt := record, p.doc.UpdatedAt
	record.Group, record.Node, record.Active = reservation.Group, reservation.Node, true
	record.Failures++
	record.LastReason = strings.TrimSpace(reason)
	if record.LastReason == "" {
		record.LastReason = "account_failed"
	}
	record.CooldownUntil = time.Now().Add(time.Duration(p.settings.FailureCooldownSeconds) * time.Second).UnixMilli()
	p.doc.Nodes[key] = record
	delete(p.doc.Reservations, holder)
	if errSave := p.persistLocked(); errSave != nil {
		if recordExisted {
			p.doc.Nodes[key] = previousRecord
		} else {
			delete(p.doc.Nodes, key)
		}
		p.doc.Reservations[holder] = reservation
		p.doc.UpdatedAt = previousUpdatedAt
		return errSave
	}
	return nil
}

func (p *ProxyPool) Reconcile(active map[string]struct{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	previous := make(map[string]proxyPoolReservation, len(p.doc.Reservations))
	for holder, reservation := range p.doc.Reservations {
		previous[holder] = reservation
	}
	previousUpdatedAt := p.doc.UpdatedAt
	for holder := range p.doc.Reservations {
		if _, exists := active[holder]; !exists {
			delete(p.doc.Reservations, holder)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if errSave := p.persistLocked(); errSave != nil {
		p.doc.Reservations = previous
		p.doc.UpdatedAt = previousUpdatedAt
		return errSave
	}
	return nil
}
