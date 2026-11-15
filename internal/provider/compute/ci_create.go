package compute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
)

// Capacity counters may describe a whole IPv6 subnet and exceed uint64.
// Accept only bounded JSON integers, never quoted, fractional, or exponent
// values, and compare them without converting through floating point.
func ciCreateIPCount(raw json.RawMessage, version int) (*big.Int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 39 || (len(raw) > 1 && raw[0] == '0') {
		return nil, fmt.Errorf("invalid CI network capacity count")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("invalid CI network capacity count")
		}
	}
	var bits uint
	switch version {
	case 4:
		bits = 32
	case 6:
		bits = 128
	default:
		return nil, fmt.Errorf("invalid CI network capacity version")
	}
	n, ok := new(big.Int).SetString(string(raw), 10)
	if !ok || n.Cmp(new(big.Int).Lsh(big.NewInt(1), bits)) > 0 {
		return nil, fmt.Errorf("invalid CI network capacity count")
	}
	return n, nil
}

// Do not return provider bodies or transport causes through errors.As/Unwrap.
// Only these fixed, nonsecret sentinels retain machine-readable identity.
func ciCreatePublicError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("compute CI runner creation canceled: %w", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("compute CI runner creation timed out: %w", context.DeadlineExceeded)
	case errors.Is(err, ErrCIRunnerOwnership):
		return fmt.Errorf("compute CI runner ownership proof failed: %w", ErrCIRunnerOwnership)
	default:
		return fmt.Errorf("compute CI runner creation failed")
	}
}

// CIRunnerCreateStage identifies one mutation stage of CreateCIRunner.
type CIRunnerCreateStage string

const (
	CIRunnerCreateSecurityGroup CIRunnerCreateStage = "security_group"
	CIRunnerCreatePort          CIRunnerCreateStage = "port"
	CIRunnerCreateServer        CIRunnerCreateStage = "server"
)

// CIRunnerCreateRequest is the complete, already-admitted creation intent for
// one CI runner operation. The caller MUST hold a durable control-plane
// one-shot creation fence and an independent enforcer lease with funding
// reserved BEFORE calling CreateCIRunner: this SDK performs no deduplication,
// no durable fencing, and no cleanup of its own.
//
// BeforeMutation is invoked synchronously exactly once immediately before
// each stage POST, with a defensive copy of the resources already
// acknowledged. The trusted caller MUST durably fence the stage — journal the
// intent, revalidate lease and funding — before returning nil; the SDK cannot
// prove that durability. A nil callback is invalid input and causes zero
// provider I/O. A callback error or a dead context prevents that stage's POST
// and every later mutation while preserving previously acknowledged IDs.
type CIRunnerCreateRequest struct {
	Scope          CIRunnerScope
	FlavorID       string
	ImageID        string
	VCPUs          int
	MemoryMiB      int
	DiskGiB        int
	UserData       string
	BeforeMutation func(context.Context, CIRunnerCreateStage, []CIRunnerResource) error
}

// CIRunnerCreateResult reports every provider resource ID acknowledged by a
// POST, in creation order, plus conservative settlement state.
//
// CreationSettled is true only when all three POST acknowledgements AND every
// fresh direct ownership/shape proof succeeded. It starts true and is cleared
// before the first POST, so any error after a mutation began — including a
// 4xx or a transport failure that may still have created the resource —
// leaves it false forever. Partial IDs in Resources must be retained by the
// caller; the independent observer must discover any late-appearing owned
// resource. This SDK never retries a POST, never deletes or stops anything,
// and never adopts pre-existing resources.
type CIRunnerCreateResult struct {
	Resources         []CIRunnerResource
	MutationAttempted bool
	CreationSettled   bool
}

const (
	ciCreateMaxUserDataBytes = 49149 // base64 of this stays under Nova's 64KiB limit
	ciCreateMemoryPerVCPU    = 4096
)

func validateCIRunnerCreateInput(in CIRunnerCreateRequest) error {
	if !ciRunnerScopeID.MatchString(in.FlavorID) || !ciRunnerScopeID.MatchString(in.ImageID) {
		return fmt.Errorf("invalid CI runner create request")
	}
	if (in.VCPUs != 2 && in.VCPUs != 4) || in.MemoryMiB != in.VCPUs*ciCreateMemoryPerVCPU || in.DiskGiB <= 0 {
		return fmt.Errorf("invalid CI runner create request")
	}
	if len(in.UserData) == 0 || len(in.UserData) > ciCreateMaxUserDataBytes {
		return fmt.Errorf("invalid CI runner create request")
	}
	if in.BeforeMutation == nil {
		return fmt.Errorf("invalid CI runner create request")
	}
	return nil
}

// CreateCIRunner provisions the security group, Neutron port, and Nova
// server for one CI runner operation. Every stage is a single POST with no
// retry; every acknowledgement is verified by a fresh direct read before the
// next mutation. All provider I/O is bounded by ctx.
//
// The returned error is sanitized: provider bodies, transport causes, and
// request URLs never escape through the error chain. Only context
// cancellation/deadline and ownership sentinels keep machine-readable
// identity.
func (c *Client) CreateCIRunner(ctx context.Context, in CIRunnerCreateRequest) (result CIRunnerCreateResult, err error) {
	defer func() { err = ciCreatePublicError(err) }()
	result = CIRunnerCreateResult{CreationSettled: true}
	if err := c.validateCIRunnerScope(in.Scope); err != nil {
		return result, err
	}
	if err := validateCIRunnerCreateInput(in); err != nil {
		return result, err
	}
	s, err := c.authenticate(ctx)
	if err != nil {
		return result, fmt.Errorf("compute CI runner create session failed: %w", err)
	}

	// Read-only preflight: exact catalog/network/capacity proofs plus a
	// complete fresh inventory proving nothing for this operation exists.
	if err := c.ciCreateCheckFlavor(ctx, s, in); err != nil {
		return result, err
	}
	if err := c.ciCreateCheckImage(ctx, s, in); err != nil {
		return result, err
	}
	subnetID, err := c.ciCreateCheckNetwork(ctx, s, in.Scope)
	if err != nil {
		return result, err
	}
	obs, err := c.ObserveCIRunner(ctx, in.Scope, nil)
	if err != nil {
		return result, fmt.Errorf("compute CI runner create inventory failed: %w", err)
	}
	if !obs.DiscoveryComplete || len(obs.Discovered) != 0 || len(obs.Reads) != 0 || len(obs.ServerPorts) != 0 {
		return result, fmt.Errorf("%w: CI runner inventory is not provably empty", ErrCIRunnerOwnership)
	}

	fail := func(stage string, err error) (CIRunnerCreateResult, error) {
		return result, fmt.Errorf("compute CI runner create %s failed: %w", stage, err)
	}
	// fence runs the caller's durable-journal seam once, immediately before
	// each stage POST. It receives a defensive copy of the resources already
	// acknowledged so the callback cannot mutate SDK state. Any callback
	// failure or dead context stops this and all later stages. Settlement
	// flips only here — after the fence accepted and the context is live —
	// so a failure before the first POST still reports CreationSettled.
	fence := func(stage CIRunnerCreateStage) error {
		snapshot := append([]CIRunnerResource(nil), result.Resources...)
		if err := in.BeforeMutation(ctx, stage, snapshot); err != nil {
			return fmt.Errorf("compute CI runner create %s admission fence failed", stage)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("compute CI runner create %s admission fence failed: %w", stage, err)
		}
		result.MutationAttempted = true
		result.CreationSettled = false
		return nil
	}

	if err := fence(CIRunnerCreateSecurityGroup); err != nil {
		return result, err
	}
	groupID, err := c.ciCreateSecurityGroup(ctx, s, in.Scope, &result)
	if err != nil {
		return fail("security group", err)
	}
	if err := fence(CIRunnerCreatePort); err != nil {
		return result, err
	}
	portID, err := c.ciCreatePort(ctx, s, in.Scope, groupID, subnetID, &result)
	if err != nil {
		return fail("port", err)
	}
	if err := fence(CIRunnerCreateServer); err != nil {
		return result, err
	}
	if _, err := c.ciCreateServer(ctx, s, in, portID, &result); err != nil {
		return fail("server", err)
	}
	result.CreationSettled = true
	return result, nil
}

func (c *Client) ciCreateCheckFlavor(ctx context.Context, s session, in CIRunnerCreateRequest) error {
	var out struct {
		Flavor struct {
			ID   string `json:"id"`
			VCPU int    `json:"vcpus"`
			RAM  int    `json:"ram"`
			Disk int    `json:"disk"`
		} `json:"flavor"`
	}
	if err := c.get(ctx, s.ComputeURL+"/flavors/"+in.FlavorID, &out); err != nil {
		return fmt.Errorf("compute CI runner create flavor check failed: %w", err)
	}
	f := out.Flavor
	if f.ID != in.FlavorID || f.VCPU != in.VCPUs || f.RAM != in.MemoryMiB || f.Disk != in.DiskGiB {
		return fmt.Errorf("compute CI runner create flavor does not match the admitted shape")
	}
	return nil
}

func (c *Client) ciCreateCheckImage(ctx context.Context, s session, in CIRunnerCreateRequest) error {
	var out struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		Architecture string `json:"architecture"`
		OSDistro     string `json:"os_distro"`
		OSVersion    string `json:"os_version"`
	}
	if err := c.get(ctx, s.ImageURL+"/v2/images/"+in.ImageID, &out); err != nil {
		return fmt.Errorf("compute CI runner create image check failed: %w", err)
	}
	if out.ID != in.ImageID || out.Status != "active" ||
		out.Architecture != "x86_64" || out.OSDistro != "ubuntu" || out.OSVersion != "24.04" {
		return fmt.Errorf("compute CI runner create image is not the admitted Ubuntu 24.04 x86_64 image")
	}
	return nil
}

// ciCreateCheckNetwork proves the pinned network is active, externally
// routed, administratively up, and has at least one IPv4 subnet with free
// capacity. It returns the lexicographically first eligible IPv4 subnet.
func (c *Client) ciCreateCheckNetwork(ctx context.Context, s session, scope CIRunnerScope) (string, error) {
	var out struct {
		Network *struct {
			ID             string   `json:"id"`
			Status         string   `json:"status"`
			AdminStateUp   bool     `json:"admin_state_up"`
			RouterExternal bool     `json:"router:external"`
			Subnets        []string `json:"subnets"`
		} `json:"network"`
	}
	if err := c.get(ctx, s.NetworkURL+"/v2.0/networks/"+scope.NetworkID, &out); err != nil {
		return "", fmt.Errorf("compute CI runner create network check failed: %w", err)
	}
	n := out.Network
	if n == nil || n.ID != scope.NetworkID || n.Status != "ACTIVE" || !n.AdminStateUp || !n.RouterExternal {
		return "", fmt.Errorf("compute CI runner create network is not active and externally routed")
	}
	subnets := make(map[string]bool, len(n.Subnets))
	for _, id := range n.Subnets {
		if !ciRunnerScopeID.MatchString(id) {
			return "", fmt.Errorf("compute CI runner create network has an invalid subnet")
		}
		if subnets[id] {
			return "", fmt.Errorf("compute CI runner create network lists a subnet twice")
		}
		subnets[id] = true
	}
	if len(subnets) == 0 {
		return "", fmt.Errorf("compute CI runner create network has no subnets")
	}

	var avail struct {
		Availability *struct {
			NetworkID string `json:"network_id"`
			Entries   *[]struct {
				IPVersion *int            `json:"ip_version"`
				TotalIPs  json.RawMessage `json:"total_ips"`
				UsedIPs   json.RawMessage `json:"used_ips"`
				SubnetID  *string         `json:"subnet_id"`
			} `json:"subnet_ip_availability"`
		} `json:"network_ip_availability"`
	}
	if err := c.get(ctx, s.NetworkURL+"/v2.0/network-ip-availabilities/"+scope.NetworkID, &avail); err != nil {
		return c.ciCreatePublicIPv4Subnet(ctx, s, scope, subnets)
	}
	a := avail.Availability
	if a != nil && a.NetworkID != scope.NetworkID {
		return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
	}
	if a == nil || a.Entries == nil || len(*a.Entries) == 0 {
		return c.ciCreatePublicIPv4Subnet(ctx, s, scope, subnets)
	}
	eligible := make([]string, 0, len(*a.Entries))
	seen := make(map[string]bool)
	for _, e := range *a.Entries {
		if e.IPVersion == nil || (*e.IPVersion != 4 && *e.IPVersion != 6) ||
			e.TotalIPs == nil || e.UsedIPs == nil || e.SubnetID == nil ||
			!ciRunnerScopeID.MatchString(*e.SubnetID) || !subnets[*e.SubnetID] || seen[*e.SubnetID] {
			return "", fmt.Errorf("compute CI runner create capacity entry is malformed or duplicated")
		}
		seen[*e.SubnetID] = true
		total, err := ciCreateIPCount(e.TotalIPs, *e.IPVersion)
		if err != nil {
			return "", err
		}
		used, err := ciCreateIPCount(e.UsedIPs, *e.IPVersion)
		if err != nil {
			return "", err
		}
		if used.Cmp(total) > 0 {
			return "", fmt.Errorf("compute CI runner create capacity exceeds its subnet")
		}
		// IPv6 capacity is never IPv4 proof; only a valid IPv4 entry with
		// free addresses makes a subnet eligible.
		if *e.IPVersion == 4 && new(big.Int).Sub(total, used).Sign() > 0 {
			eligible = append(eligible, *e.SubnetID)
		}
	}
	if len(eligible) == 0 {
		return "", fmt.Errorf("compute CI runner create network has no IPv4 capacity")
	}
	sort.Strings(eligible)
	return eligible[0], nil
}

type ciPublicRegionNetwork struct {
	NetworkID string          `json:"network_id"`
	IPv4      json.RawMessage `json:"available_ips_v4"`
}

type ciPublicRegion struct {
	Region      string                  `json:"region"`
	Available   *bool                   `json:"is_available"`
	Maintenance *bool                   `json:"is_maintenance"`
	ReadOnly    *bool                   `json:"is_read_only"`
	OutOfStock  *bool                   `json:"is_out_of_stock"`
	Status      *string                 `json:"dynamic_status"`
	AgeSeconds  json.RawMessage         `json:"dynamic_age_seconds"`
	Networks    []ciPublicRegionNetwork `json:"networks"`
}

// ciPublicSmallInt accepts only a bounded non-negative JSON integer, never a
// quoted, fractional, or exponent value.
func ciPublicSmallInt(raw json.RawMessage, limit int64) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 18 {
		return 0, false
	}
	var n int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
		if n > limit {
			return 0, false
		}
	}
	return n, true
}

// ciCreatePublicIPv4Subnet proves pinned-network IPv4 capacity through the
// provider's public region feed when Neutron publishes no availability
// records, which HostVDS does not for shared external networks. It requires
// exactly one healthy entry for the configured region with a fresh dynamic
// status and the pinned network listed exactly once with positive IPv4
// capacity, then returns the first subnet the authenticated API reports as
// IPv4. The feed request carries no credentials.
func (c *Client) ciCreatePublicIPv4Subnet(ctx context.Context, s session, scope CIRunnerScope, subnets map[string]bool) (string, error) {
	var candidates []string
	if override := strings.TrimSpace(
		os.Getenv("CANTER_COMPUTE_REGIONS_FEED_URL")); override != "" {
		candidates = append(candidates, override)
	}
	u, err := url.Parse(s.NetworkURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("compute CI runner create capacity check failed")
	}
	feed := *u
	feed.Path, feed.RawPath, feed.RawQuery, feed.Fragment = "/api/regions/", "", "", ""
	candidates = append(candidates, feed.String())
	// HostVDS serves the public feed on the apex host while OpenStack APIs
	// live on os-api.<domain>; derive the apex form as a second candidate.
	host := u.Hostname()
	if strings.HasPrefix(host, "os-api.") {
		apex := *u
		apex.Host = strings.TrimPrefix(host, "os-api.")
		if u.Port() != "" {
			apex.Host = apex.Host + ":" + u.Port()
		}
		apex.Path, apex.RawPath = "/api/regions/", ""
		candidates = append(candidates, apex.String())
	}
	var data []byte
	for _, cURL := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cURL, nil)
		if err != nil {
			continue
		}
		resp, err := c.http.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxProviderJSONBody+1))
		resp.Body.Close()
		if err == nil && len(data) <= maxProviderJSONBody {
			break
		}
		data = nil
	}
	if data == nil {
		return "", fmt.Errorf("compute CI runner create capacity check failed")
	}
	if err != nil || len(data) > maxProviderJSONBody {
		return "", fmt.Errorf("compute CI runner create capacity check failed")
	}
	var regions []ciPublicRegion
	if err := json.Unmarshal(data, &regions); err != nil {
		return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
	}
	var entry *ciPublicRegion
	matches := 0
	for i := range regions {
		if regions[i].Region == c.config.Region {
			matches++
			entry = &regions[i]
		}
	}
	if matches != 1 || entry == nil {
		return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
	}
	_, ageOK := ciPublicSmallInt(entry.AgeSeconds, 120)
	if entry.Available == nil || !*entry.Available ||
		entry.Maintenance == nil || *entry.Maintenance ||
		entry.ReadOnly == nil || *entry.ReadOnly ||
		entry.OutOfStock == nil || *entry.OutOfStock ||
		entry.Status == nil || *entry.Status != "ok" || !ageOK {
		return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
	}
	netMatches := 0
	var free int64
	for _, nw := range entry.Networks {
		if nw.NetworkID == scope.NetworkID {
			netMatches++
			n, ok := ciPublicSmallInt(nw.IPv4, math.MaxInt32)
			if !ok {
				return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
			}
			free = n
		}
	}
	if netMatches != 1 {
		return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
	}
	if free <= 0 {
		return "", fmt.Errorf("compute CI runner create network has no IPv4 capacity")
	}
	ids := make([]string, 0, len(subnets))
	for id := range subnets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var sub struct {
			Subnet *struct {
				ID        string `json:"id"`
				NetworkID string `json:"network_id"`
				IPVersion *int   `json:"ip_version"`
			} `json:"subnet"`
		}
		if err := c.get(ctx, s.NetworkURL+"/v2.0/subnets/"+id, &sub); err != nil {
			return "", fmt.Errorf("compute CI runner create subnet check failed: %w", err)
		}
		if sub.Subnet == nil || sub.Subnet.ID != id || sub.Subnet.NetworkID != scope.NetworkID || sub.Subnet.IPVersion == nil {
			return "", fmt.Errorf("compute CI runner create capacity is unavailable or untrustworthy")
		}
		if *sub.Subnet.IPVersion == 4 {
			return id, nil
		}
	}
	return "", fmt.Errorf("compute CI runner create network has no IPv4 capacity")
}

// ciCreateRetain appends an acknowledged provider ID to the result.
func ciCreateRetain(result *CIRunnerCreateResult, kind, id string, scope CIRunnerScope) error {
	if !ciRunnerScopeID.MatchString(id) {
		return fmt.Errorf("compute provider acknowledged an invalid %s ID", kind)
	}
	result.Resources = append(result.Resources, CIRunnerResource{Kind: kind, ID: id, OperationID: scope.OperationID})
	return nil
}

func (c *Client) ciCreateSecurityGroup(ctx context.Context, s session, scope CIRunnerScope, result *CIRunnerCreateResult) (string, error) {
	name := scope.ServerName + "-ci"
	tag := scope.operationTag()
	payload := map[string]any{"security_group": map[string]any{"name": name, "description": tag}}
	var ack struct {
		Group struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"security_group"`
	}
	if err := c.request(ctx, http.MethodPost, s.NetworkURL+"/v2.0/security-groups", payload, &ack); err != nil {
		return "", err
	}
	g := ack.Group
	// Retain the acknowledged candidate ID before any ownership check: a
	// mismatched acknowledgement may still name a real provider object that
	// the caller must account for. It is never deleted or stopped here.
	if err := ciCreateRetain(result, "security_group", g.ID, scope); err != nil {
		return "", err
	}
	if g.Name != name || g.Description != tag {
		return "", fmt.Errorf("compute security group acknowledgement ownership mismatch")
	}

	// Fresh direct read: exact ownership plus a complete rule list with no
	// ingress. Unknown or missing rule data fails closed.
	var detail struct {
		Group struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Rules       *[]struct {
				Direction *string `json:"direction"`
			} `json:"security_group_rules"`
		} `json:"security_group"`
	}
	if err := c.get(ctx, s.NetworkURL+"/v2.0/security-groups/"+g.ID, &detail); err != nil {
		return "", err
	}
	d := detail.Group
	if d.ID != g.ID || d.Name != name || d.Description != tag || d.Rules == nil {
		return "", fmt.Errorf("compute security group ownership revalidation failed")
	}
	for _, rule := range *d.Rules {
		if rule.Direction == nil || *rule.Direction != "egress" {
			return "", fmt.Errorf("compute security group has ingress or unverifiable rules")
		}
	}
	return g.ID, nil
}

func (c *Client) ciCreatePort(ctx context.Context, s session, scope CIRunnerScope, groupID, subnetID string, result *CIRunnerCreateResult) (string, error) {
	name := scope.ServerName + "-ci"
	tag := scope.operationTag()
	// HostVDS policy forbids tenants from setting fixed_ips,
	// port_security_enabled, or allowed_address_pairs on shared external
	// networks; auto-allocation and the secure defaults are verified on
	// read-back instead.
	payload := map[string]any{"port": map[string]any{
		"name": name, "description": tag,
		"network_id":      scope.NetworkID,
		"admin_state_up":  true,
		"security_groups": []string{groupID},
	}}
	var ack struct {
		Port struct {
			ID string `json:"id"`
		} `json:"port"`
	}
	if err := c.request(ctx, http.MethodPost, s.NetworkURL+"/v2.0/ports", payload, &ack); err != nil {
		return "", err
	}
	if err := ciCreateRetain(result, "port", ack.Port.ID, scope); err != nil {
		return "", err
	}

	// Fresh direct read: the port must still be unattached, administratively
	// up, pinned to the exact network, secured by exactly the new group,
	// carrying exactly one valid global-unicast IPv4 fixed address inside
	// the chosen subnet, and allow no address pairs. Additional IPv6 fixed
	// addresses from dual-stack auto-allocation are expected and ignored.
	var detail struct {
		Port struct {
			ID                  string   `json:"id"`
			Name                string   `json:"name"`
			Description         string   `json:"description"`
			NetworkID           string   `json:"network_id"`
			DeviceID            string   `json:"device_id"`
			AdminStateUp        *bool    `json:"admin_state_up"`
			PortSecurityEnabled *bool    `json:"port_security_enabled"`
			SecurityGroups      []string `json:"security_groups"`
			FixedIPs            []struct {
				SubnetID  string `json:"subnet_id"`
				IPAddress string `json:"ip_address"`
			} `json:"fixed_ips"`
			AllowedAddressPairs *[]json.RawMessage `json:"allowed_address_pairs"`
		} `json:"port"`
	}
	if err := c.get(ctx, s.NetworkURL+"/v2.0/ports/"+ack.Port.ID, &detail); err != nil {
		return "", err
	}
	p := detail.Port
	if p.ID != ack.Port.ID || p.Name != name || p.Description != tag ||
		p.NetworkID != scope.NetworkID || p.DeviceID != "" ||
		p.AdminStateUp == nil || !*p.AdminStateUp ||
		p.PortSecurityEnabled == nil || !*p.PortSecurityEnabled ||
		len(p.SecurityGroups) != 1 || p.SecurityGroups[0] != groupID ||
		p.AllowedAddressPairs == nil || len(*p.AllowedAddressPairs) != 0 ||
		len(p.FixedIPs) == 0 {
		return "", fmt.Errorf("compute port ownership revalidation failed")
	}
	matched := 0
	for _, fixed := range p.FixedIPs {
		if fixed.SubnetID != subnetID {
			continue
		}
		addr, addrErr := netip.ParseAddr(fixed.IPAddress)
		if addrErr != nil || !addr.Is4() || !addr.IsGlobalUnicast() {
			return "", fmt.Errorf("compute port fixed address is not the admitted IPv4 subnet address")
		}
		matched++
	}
	if matched != 1 {
		return "", fmt.Errorf("compute port fixed address is not the admitted IPv4 subnet address")
	}
	return ack.Port.ID, nil
}

func (c *Client) ciCreateServer(ctx context.Context, s session, in CIRunnerCreateRequest, portID string, result *CIRunnerCreateResult) (string, error) {
	metadata := map[string]string{
		"canter.sandbox":   "ci-" + in.Scope.OperationID,
		"canter.managed":   "true",
		"canter.operation": in.Scope.OperationID,
		"canter.resource":  in.Scope.ServerName,
	}
	payload := map[string]any{"server": map[string]any{
		"name":      in.Scope.ServerName,
		"flavorRef": in.FlavorID,
		"imageRef":  in.ImageID,
		"networks":  []map[string]string{{"port": portID}},
		"metadata":  metadata,
		"user_data": base64.StdEncoding.EncodeToString([]byte(in.UserData)),
	}}
	var ack struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	if err := c.request(ctx, http.MethodPost, s.ComputeURL+"/servers", payload, &ack); err != nil {
		return "", err
	}
	if err := ciCreateRetain(result, "server", ack.Server.ID, in.Scope); err != nil {
		return "", err
	}

	// Fresh direct read must prove exact scope ownership AND the exact
	// admitted flavor and image identities — equal vCPU/RAM alone would
	// accept a different-priced flavor or the wrong OS. The server may still
	// be BUILDing; activation is the caller's later concern.
	var detail struct {
		Server struct {
			ciRunnerItem
			Flavor struct {
				ID string `json:"id"`
			} `json:"flavor"`
			Image struct {
				ID string `json:"id"`
			} `json:"image"`
		} `json:"server"`
	}
	if err := c.get(ctx, s.ComputeURL+"/servers/"+ack.Server.ID, &detail); err != nil {
		return "", err
	}
	item := detail.Server.ciRunnerItem
	if item.ID != ack.Server.ID || !in.Scope.serverOwned(item) {
		return "", fmt.Errorf("compute server ownership revalidation failed")
	}
	if detail.Server.Flavor.ID != in.FlavorID || detail.Server.Image.ID != in.ImageID {
		return "", fmt.Errorf("compute server flavor or image does not match the admitted request")
	}
	shape, err := c.ServerShape(ctx, ack.Server.ID)
	if err != nil {
		return "", err
	}
	if shape.VCPU != in.VCPUs || shape.Memory != in.MemoryMiB {
		return "", fmt.Errorf("compute server allocated shape does not match the admitted flavor")
	}
	// Re-prove the catalog flavor is still the admitted exact shape; a
	// flavor mutated between preflight and allocation must not pass.
	if err := c.ciCreateCheckFlavor(ctx, s, in); err != nil {
		return "", err
	}
	return ack.Server.ID, nil
}
