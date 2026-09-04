package compute

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/internal/computeclass"
)

type Client struct {
	http    *http.Client
	config  config
	mu      sync.Mutex
	session session
}

type config struct {
	AuthURL, Username, Password, ProjectName, ProjectDomainID, Region string
}

type session struct {
	Token, ComputeURL, ImageURL, NetworkURL string
	Expires                                 time.Time
}

type ProbeResult struct {
	OK       bool          `json:"ok"`
	Latency  time.Duration `json:"latency"`
	Servers  int           `json:"servers"`
	Shapes   int           `json:"shapes"`
	Images   int           `json:"images"`
	Networks int           `json:"networks"`
	Error    string        `json:"error,omitempty"`
}

type Shape struct {
	ID, Name         string
	VCPU, Memory, GB int
}

type Server struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Status    string               `json:"status"`
	Metadata  map[string]string    `json:"metadata"`
	Fault     *Fault               `json:"fault,omitempty"`
	Addresses map[string][]Address `json:"addresses,omitempty"`
}

type Address struct {
	Addr    string `json:"addr"`
	Version int    `json:"version"`
	Type    string `json:"OS-EXT-IPS:type"`
}

func (s Server) IPv4() string {
	for _, addresses := range s.Addresses {
		for _, address := range addresses {
			if address.Version == 4 && address.Addr != "" {
				return address.Addr
			}
		}
	}
	return ""
}

type Fault struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

type SecurityPolicy struct {
	ID, PortID, RuleID string
	Port               int
}

type ManagedServerRequest struct {
	Name, Sandbox, OperationID, FlavorID, ImageID, NetworkID, UserData string
}

type ManagedTCPExposureRequest struct {
	ServerID, Name, Ownership string
	Port                      int
}

type DuplicateManagedResourceError struct {
	Kind, Identity string
	Count          int
}

func (e *DuplicateManagedResourceError) Error() string {
	return fmt.Sprintf("managed %s %q is ambiguous: found %d matches", e.Kind, e.Identity, e.Count)
}

func IsDuplicateManagedResource(err error) bool {
	var duplicate *DuplicateManagedResourceError
	return errors.As(err, &duplicate)
}

type AmbiguousManagedResourceError struct {
	Kind, Identity string
	Cause          error
}

func (e *AmbiguousManagedResourceError) Error() string {
	return fmt.Sprintf("managed %s %q has an unresolved provider outcome: %v", e.Kind, e.Identity, e.Cause)
}

func (e *AmbiguousManagedResourceError) Unwrap() error { return e.Cause }

func IsAmbiguousManagedResource(err error) bool {
	var ambiguous *AmbiguousManagedResourceError
	return errors.As(err, &ambiguous)
}

func IsNotFound(err error) bool { return isHTTPStatus(err, http.StatusNotFound) }

// HTTPError preserves the provider status code so reconciliation can make
// idempotent decisions without parsing an error string.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("compute request failed with HTTP %d: %s", e.StatusCode, e.Body)
}

func isHTTPStatus(err error, status int) bool {
	var responseErr *HTTPError
	return errors.As(err, &responseErr) && responseErr.StatusCode == status
}

func NewFromEnv() (*Client, error) {
	cfg := config{
		AuthURL: os.Getenv("CANTER_COMPUTE_AUTH_URL"), Username: os.Getenv("CANTER_COMPUTE_USERNAME"), Password: os.Getenv("CANTER_COMPUTE_PASSWORD"),
		ProjectName: os.Getenv("CANTER_COMPUTE_PROJECT"), ProjectDomainID: os.Getenv("CANTER_COMPUTE_PROJECT_DOMAIN"), Region: os.Getenv("CANTER_COMPUTE_REGION"),
	}
	if cfg.AuthURL == "" || cfg.Username == "" || cfg.Password == "" || cfg.ProjectName == "" || cfg.ProjectDomainID == "" || cfg.Region == "" {
		return nil, fmt.Errorf("compute credentials are incomplete")
	}
	return &Client{config: cfg, http: &http.Client{Timeout: 18 * time.Second}}, nil
}

func (c *Client) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	s, err := c.authenticate(ctx)
	if err != nil {
		return ProbeResult{Latency: time.Since(start), Error: err.Error()}
	}
	var servers []Server
	var shapes []Shape
	var images []image
	var networks []network
	er := make(chan error, 4)
	go func() {
		var x struct {
			Servers []Server `json:"servers"`
		}
		e := c.get(ctx, s.ComputeURL+"/servers/detail", &x)
		servers = x.Servers
		er <- e
	}()
	go func() {
		var x flavorList
		e := c.get(ctx, s.ComputeURL+"/flavors/detail", &x)
		shapes = x.shapes()
		er <- e
	}()
	go func() {
		var x struct {
			Images []image `json:"images"`
		}
		e := c.get(ctx, s.ImageURL+"/v2/images?limit=100", &x)
		images = x.Images
		er <- e
	}()
	go func() {
		var x struct {
			Networks []network `json:"networks"`
		}
		e := c.get(ctx, s.NetworkURL+"/v2.0/networks", &x)
		networks = x.Networks
		er <- e
	}()
	for range 4 {
		if err := <-er; err != nil {
			return ProbeResult{Latency: time.Since(start), Error: err.Error()}
		}
	}
	return ProbeResult{OK: true, Latency: time.Since(start), Servers: len(servers), Shapes: len(shapes), Images: len(images), Networks: len(networks)}
}

func (c *Client) Resolve(ctx context.Context, class, imageAlias string) (Shape, string, []string, error) {
	s, err := c.authenticate(ctx)
	if err != nil {
		return Shape{}, "", nil, err
	}
	var fs flavorList
	if err := c.get(ctx, s.ComputeURL+"/flavors/detail", &fs); err != nil {
		return Shape{}, "", nil, err
	}
	shapes := fs.shapes()
	sort.Slice(shapes, func(i, j int) bool {
		if shapes[i].Memory != shapes[j].Memory {
			return shapes[i].Memory < shapes[j].Memory
		}
		if shapes[i].VCPU != shapes[j].VCPU {
			return shapes[i].VCPU < shapes[j].VCPU
		}
		return shapes[i].GB < shapes[j].GB
	})
	index, ok := computeclass.Index(strings.ToLower(class))
	if !ok || index >= len(shapes) {
		return Shape{}, "", nil, computeclass.UnsupportedError(class)
	}
	var imgs struct {
		Images []image `json:"images"`
	}
	if err := c.get(ctx, s.ImageURL+"/v2/images?limit=100", &imgs); err != nil {
		return Shape{}, "", nil, err
	}
	want := normalizeImage(imageAlias)
	imageID := ""
	for _, img := range imgs.Images {
		n := strings.ToLower(img.Name)
		norm := normalizeImage(n)
		if img.Status == "active" && !strings.HasPrefix(n, "old_") && strings.Contains(norm, want) {
			imageID = img.ID
			break
		}
	}
	if imageID == "" {
		return Shape{}, "", nil, fmt.Errorf("unsupported compute image %q", imageAlias)
	}
	var nets struct {
		Networks []network `json:"networks"`
	}
	if err := c.get(ctx, s.NetworkURL+"/v2.0/networks", &nets); err != nil {
		return Shape{}, "", nil, err
	}
	sort.Slice(nets.Networks, func(i, j int) bool { return nets.Networks[i].Name < nets.Networks[j].Name })
	networkIDs := make([]string, 0, len(nets.Networks))
	for _, n := range nets.Networks {
		lower := strings.ToLower(n.Name)
		if n.External && n.Status == "ACTIVE" && !strings.Contains(lower, "reserve") && !strings.Contains(lower, "notworking") && !strings.Contains(lower, "ipv6") {
			networkIDs = append(networkIDs, n.ID)
		}
	}
	if len(networkIDs) == 0 {
		return Shape{}, "", nil, fmt.Errorf("no usable compute network")
	}
	return shapes[index], imageID, networkIDs, nil
}

func normalizeImage(value string) string {
	return strings.NewReplacer("-", "", ".", "", "_", "", "amd64", "").Replace(strings.ToLower(value))
}

func (c *Client) Create(ctx context.Context, name, sandbox, flavorID, imageID, networkID, userData string) (Server, error) {
	return c.CreateManaged(ctx, ManagedServerRequest{Name: name, Sandbox: sandbox, FlavorID: flavorID, ImageID: imageID, NetworkID: networkID, UserData: userData})
}

func (c *Client) CreateManaged(ctx context.Context, input ManagedServerRequest) (Server, error) {
	s, err := c.authenticate(ctx)
	if err != nil {
		return Server{}, err
	}
	metadata := map[string]string{"canter.sandbox": input.Sandbox, "canter.managed": "true"}
	if input.OperationID != "" {
		metadata["canter.operation"] = input.OperationID
		metadata["canter.resource"] = input.Name
	}
	payload := map[string]any{"server": map[string]any{
		"name": input.Name, "flavorRef": input.FlavorID, "imageRef": input.ImageID,
		"networks":  []map[string]string{{"uuid": input.NetworkID}},
		"metadata":  metadata,
		"user_data": base64.StdEncoding.EncodeToString([]byte(input.UserData)),
	}}
	var out struct {
		Server Server `json:"server"`
	}
	if err := c.request(ctx, http.MethodPost, s.ComputeURL+"/servers", payload, &out); err != nil {
		return Server{}, err
	}
	return out.Server, nil
}

// FindManagedServers returns exact matches for a durable Canter creation
// intent. Provider name filters are not assumed to be exact, so the response
// is filtered again locally, including all managed metadata.
func (c *Client) FindManagedServers(ctx context.Context, sandbox, operationID, name string) ([]Server, error) {
	s, err := c.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("name", name)
	var out struct {
		Servers []Server `json:"servers"`
	}
	if err := c.get(ctx, s.ComputeURL+"/servers/detail?"+query.Encode(), &out); err != nil {
		return nil, err
	}
	matches := make([]Server, 0, len(out.Servers))
	for _, server := range out.Servers {
		if server.Name != name || server.Metadata["canter.managed"] != "true" || server.Metadata["canter.sandbox"] != sandbox || server.Metadata["canter.operation"] != operationID || server.Metadata["canter.resource"] != name {
			continue
		}
		matches = append(matches, server)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	return matches, nil
}

func (c *Client) Server(ctx context.Context, id string) (Server, error) {
	s, err := c.authenticate(ctx)
	if err != nil {
		return Server{}, err
	}
	var out struct {
		Server Server `json:"server"`
	}
	err = c.get(ctx, s.ComputeURL+"/servers/"+id, &out)
	return out.Server, err
}

func (c *Client) WaitActive(ctx context.Context, id string) (Server, error) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		server, err := c.Server(ctx, id)
		if err != nil {
			return Server{}, err
		}
		switch server.Status {
		case "ACTIVE":
			return server, nil
		case "ERROR":
			if server.Fault != nil {
				return Server{}, fmt.Errorf("compute resource entered ERROR state: code=%d message=%s", server.Fault.Code, strings.TrimSpace(server.Fault.Message))
			}
			return Server{}, fmt.Errorf("compute resource entered ERROR state without a fault message")
		}
		select {
		case <-ctx.Done():
			return Server{}, ctx.Err()
		case <-tick.C:
		}
	}
}

func IsNetworkExhausted(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no fixed ip addresses available")
}

func (c *Client) Delete(ctx context.Context, id string) error {
	s, err := c.authenticate(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.ComputeURL+"/servers/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", s.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("compute delete failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

type networkPort struct {
	ID             string   `json:"id"`
	SecurityGroups []string `json:"security_groups"`
}

type securityRule struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Protocol  string `json:"protocol"`
	Min       int    `json:"port_range_min"`
	Max       int    `json:"port_range_max"`
}

type securityGroup struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Rules       []securityRule `json:"security_group_rules"`
}

func (c *Client) ExposeTCP(ctx context.Context, serverID, name string, portNumber int) (SecurityPolicy, error) {
	ownershipDigest := sha256.Sum256([]byte(serverID + "\x00" + name))
	return c.ExposeManagedTCP(ctx, ManagedTCPExposureRequest{ServerID: serverID, Name: name, Ownership: fmt.Sprintf("legacy-sha256:%x", ownershipDigest[:]), Port: portNumber})
}

func (c *Client) ExposeManagedTCP(ctx context.Context, input ManagedTCPExposureRequest) (SecurityPolicy, error) {
	serverID, name, portNumber := input.ServerID, input.Name, input.Port
	if portNumber < 1 || portNumber > 65535 {
		return SecurityPolicy{}, fmt.Errorf("invalid TCP port")
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(input.Ownership) == "" {
		return SecurityPolicy{}, fmt.Errorf("managed TCP exposure requires name and ownership")
	}
	description := managedPolicyDescription(input.Ownership)
	s, err := c.authenticate(ctx)
	if err != nil {
		return SecurityPolicy{}, err
	}
	port, err := c.findServerPort(ctx, s, serverID)
	if err != nil {
		return SecurityPolicy{}, err
	}
	groups, err := c.findSecurityGroups(ctx, s, name, description)
	if err != nil {
		return SecurityPolicy{}, err
	}
	if len(groups) > 1 {
		return SecurityPolicy{}, &DuplicateManagedResourceError{Kind: "network policy", Identity: name, Count: len(groups)}
	}
	var group securityGroup
	if len(groups) == 1 {
		group = groups[0]
	} else {
		group, err = c.createSecurityGroup(ctx, s, name, description)
		if err != nil || group.ID == "" {
			reconciled, lookupErr := c.findSecurityGroups(ctx, s, name, description)
			if lookupErr != nil {
				return SecurityPolicy{}, fmt.Errorf("create network policy failed (%v) and reconciliation failed: %w", err, lookupErr)
			}
			switch len(reconciled) {
			case 0:
				if err != nil {
					return SecurityPolicy{}, &AmbiguousManagedResourceError{Kind: "network policy", Identity: name, Cause: err}
				}
				return SecurityPolicy{}, fmt.Errorf("provider returned an empty network policy")
			case 1:
				group = reconciled[0]
			default:
				return SecurityPolicy{}, &DuplicateManagedResourceError{Kind: "network policy", Identity: name, Count: len(reconciled)}
			}
		}
	}
	rules := matchingTCPRules(group, portNumber)
	if len(rules) > 1 {
		return SecurityPolicy{}, &DuplicateManagedResourceError{Kind: "network policy rule", Identity: fmt.Sprintf("%s:tcp/%d", name, portNumber), Count: len(rules)}
	}
	ruleID := ""
	if len(rules) == 1 {
		ruleID = rules[0].ID
	} else {
		ruleID, err = c.createTCPRule(ctx, s, group.ID, portNumber)
		if err != nil || ruleID == "" {
			reconciled, lookupErr := c.findSecurityGroups(ctx, s, name, description)
			if lookupErr != nil {
				return SecurityPolicy{}, fmt.Errorf("create network policy rule failed (%v) and reconciliation failed: %w", err, lookupErr)
			}
			if len(reconciled) > 1 {
				return SecurityPolicy{}, &DuplicateManagedResourceError{Kind: "network policy", Identity: name, Count: len(reconciled)}
			}
			if len(reconciled) == 1 {
				rules = matchingTCPRules(reconciled[0], portNumber)
			}
			switch len(rules) {
			case 0:
				if err != nil {
					return SecurityPolicy{}, &AmbiguousManagedResourceError{Kind: "network policy rule", Identity: fmt.Sprintf("%s:tcp/%d", name, portNumber), Cause: err}
				}
				return SecurityPolicy{}, fmt.Errorf("provider returned an empty network policy rule")
			case 1:
				ruleID = rules[0].ID
			default:
				return SecurityPolicy{}, &DuplicateManagedResourceError{Kind: "network policy rule", Identity: fmt.Sprintf("%s:tcp/%d", name, portNumber), Count: len(rules)}
			}
		}
	}
	if !containsString(port.SecurityGroups, group.ID) {
		attached := append(append([]string(nil), port.SecurityGroups...), group.ID)
		payload := map[string]any{"port": map[string]any{"security_groups": attached}}
		attachErr := c.request(ctx, http.MethodPut, s.NetworkURL+"/v2.0/ports/"+port.ID, payload, nil)
		if attachErr != nil {
			reconciled, lookupErr := c.findServerPort(ctx, s, serverID)
			if lookupErr != nil {
				return SecurityPolicy{}, fmt.Errorf("attach network policy failed (%v) and reconciliation failed: %w", attachErr, lookupErr)
			}
			if !containsString(reconciled.SecurityGroups, group.ID) {
				return SecurityPolicy{}, &AmbiguousManagedResourceError{Kind: "network policy attachment", Identity: fmt.Sprintf("%s:%s", serverID, name), Cause: attachErr}
			}
			port = reconciled
		}
	}
	return SecurityPolicy{ID: group.ID, PortID: port.ID, RuleID: ruleID, Port: portNumber}, nil
}

// FindManagedTCPExposure is deliberately lookup-only. It is used after a
// persisted in-flight exposure intent: a complete exact policy is recovered,
// while absence or partial provider state is reported without issuing another
// create or attachment mutation.
func (c *Client) FindManagedTCPExposure(ctx context.Context, input ManagedTCPExposureRequest) (SecurityPolicy, bool, error) {
	if input.Port < 1 || input.Port > 65535 || strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Ownership) == "" {
		return SecurityPolicy{}, false, fmt.Errorf("managed TCP exposure requires valid name, ownership, and port")
	}
	s, err := c.authenticate(ctx)
	if err != nil {
		return SecurityPolicy{}, false, err
	}
	port, err := c.findServerPort(ctx, s, input.ServerID)
	if err != nil {
		return SecurityPolicy{}, false, err
	}
	description := managedPolicyDescription(input.Ownership)
	groups, err := c.findSecurityGroups(ctx, s, input.Name, description)
	if err != nil {
		return SecurityPolicy{}, false, err
	}
	switch len(groups) {
	case 0:
		return SecurityPolicy{}, false, nil
	case 1:
	default:
		return SecurityPolicy{}, false, &DuplicateManagedResourceError{Kind: "network policy", Identity: input.Name, Count: len(groups)}
	}
	rules := matchingTCPRules(groups[0], input.Port)
	if len(rules) > 1 {
		return SecurityPolicy{}, false, &DuplicateManagedResourceError{Kind: "network policy rule", Identity: fmt.Sprintf("%s:tcp/%d", input.Name, input.Port), Count: len(rules)}
	}
	if len(rules) == 0 {
		return SecurityPolicy{}, false, &AmbiguousManagedResourceError{Kind: "network policy", Identity: input.Name, Cause: fmt.Errorf("owned policy exists without the intended rule")}
	}
	if !containsString(port.SecurityGroups, groups[0].ID) {
		return SecurityPolicy{}, false, &AmbiguousManagedResourceError{Kind: "network policy attachment", Identity: fmt.Sprintf("%s:%s", input.ServerID, input.Name), Cause: fmt.Errorf("owned policy exists but is not attached")}
	}
	return SecurityPolicy{ID: groups[0].ID, PortID: port.ID, RuleID: rules[0].ID, Port: input.Port}, true, nil
}

func (c *Client) findServerPort(ctx context.Context, s session, serverID string) (networkPort, error) {
	var out struct {
		Ports []networkPort `json:"ports"`
	}
	if err := c.get(ctx, s.NetworkURL+"/v2.0/ports?device_id="+url.QueryEscape(serverID), &out); err != nil {
		return networkPort{}, err
	}
	if len(out.Ports) != 1 {
		return networkPort{}, fmt.Errorf("expected one compute network port, found %d", len(out.Ports))
	}
	return out.Ports[0], nil
}

func (c *Client) findSecurityGroups(ctx context.Context, s session, name, description string) ([]securityGroup, error) {
	var out struct {
		SecurityGroups []securityGroup `json:"security_groups"`
	}
	query := url.Values{}
	query.Set("name", name)
	if err := c.get(ctx, s.NetworkURL+"/v2.0/security-groups?"+query.Encode(), &out); err != nil {
		return nil, err
	}
	groups := make([]securityGroup, 0, len(out.SecurityGroups))
	for _, group := range out.SecurityGroups {
		if group.Name == name && group.Description == description {
			groups = append(groups, group)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups, nil
}

func (c *Client) createSecurityGroup(ctx context.Context, s session, name, description string) (securityGroup, error) {
	var created struct {
		SecurityGroup securityGroup `json:"security_group"`
	}
	payload := map[string]any{"security_group": map[string]string{"name": name, "description": description}}
	err := c.request(ctx, http.MethodPost, s.NetworkURL+"/v2.0/security-groups", payload, &created)
	return created.SecurityGroup, err
}

func managedPolicyDescription(ownership string) string {
	return "Canter-managed public endpoint policy; owner=" + ownership
}

func (c *Client) createTCPRule(ctx context.Context, s session, groupID string, portNumber int) (string, error) {
	var created struct {
		SecurityGroupRule securityRule `json:"security_group_rule"`
	}
	payload := map[string]any{"security_group_rule": map[string]any{"security_group_id": groupID, "direction": "ingress", "ethertype": "IPv4", "protocol": "tcp", "port_range_min": portNumber, "port_range_max": portNumber, "remote_ip_prefix": "0.0.0.0/0"}}
	err := c.request(ctx, http.MethodPost, s.NetworkURL+"/v2.0/security-group-rules", payload, &created)
	return created.SecurityGroupRule.ID, err
}

func matchingTCPRules(group securityGroup, port int) []securityRule {
	rules := make([]securityRule, 0, len(group.Rules))
	for _, rule := range group.Rules {
		if rule.Direction == "ingress" && rule.Protocol == "tcp" && rule.Min == port && rule.Max == port {
			rules = append(rules, rule)
		}
	}
	return rules
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (c *Client) DeleteSecurityPolicy(ctx context.Context, id string) error {
	s, err := c.authenticate(ctx)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		last = c.request(ctx, http.MethodDelete, s.NetworkURL+"/v2.0/security-groups/"+id, nil, nil)
		if last == nil || isHTTPStatus(last, http.StatusNotFound) {
			return nil
		}
		if !isHTTPStatus(last, http.StatusConflict) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return last
}

type flavorList struct {
	Flavors []struct {
		ID, Name         string
		VCPUs, RAM, Disk int
	} `json:"flavors"`
}

func (f flavorList) shapes() []Shape {
	out := make([]Shape, 0, len(f.Flavors))
	for _, x := range f.Flavors {
		out = append(out, Shape{ID: x.ID, Name: x.Name, VCPU: x.VCPUs, Memory: x.RAM, GB: x.Disk})
	}
	return out
}

type image struct{ ID, Name, Status string }
type network struct {
	ID, Name, Status string
	External         bool `json:"router:external"`
}

func (c *Client) authenticate(ctx context.Context) (session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session.Token != "" && time.Until(c.session.Expires) > time.Minute {
		return c.session, nil
	}
	payload := map[string]any{"auth": map[string]any{
		"identity": map[string]any{"methods": []string{"password"}, "password": map[string]any{"user": map[string]any{"name": c.config.Username, "domain": map[string]string{"id": "default"}, "password": c.config.Password}}},
		"scope":    map[string]any{"project": map[string]any{"name": c.config.ProjectName, "domain": map[string]string{"id": c.config.ProjectDomainID}}},
	}}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.config.AuthURL, "/")+"/v3/auth/tokens", bytes.NewReader(b))
	if err != nil {
		return session{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return session{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return session{}, fmt.Errorf("compute authentication failed with HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token struct {
			ExpiresAt time.Time `json:"expires_at"`
			Catalog   []struct {
				Type      string
				Endpoints []struct{ Interface, Region, URL string }
			}
		} `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return session{}, err
	}
	s := session{Token: resp.Header.Get("X-Subject-Token"), Expires: body.Token.ExpiresAt}
	for _, svc := range body.Token.Catalog {
		for _, ep := range svc.Endpoints {
			if ep.Interface != "public" || ep.Region != c.config.Region {
				continue
			}
			switch svc.Type {
			case "compute":
				s.ComputeURL = strings.TrimRight(ep.URL, "/")
			case "image":
				s.ImageURL = strings.TrimRight(ep.URL, "/")
			case "network":
				s.NetworkURL = strings.TrimRight(ep.URL, "/")
			}
		}
	}
	if s.Token == "" || s.ComputeURL == "" || s.ImageURL == "" || s.NetworkURL == "" {
		return session{}, fmt.Errorf("compute service catalog is incomplete")
	}
	c.session = s
	return s, nil
}

func (c *Client) get(ctx context.Context, url string, target any) error {
	return c.request(ctx, http.MethodGet, url, nil, target)
}
func (c *Client) request(ctx context.Context, method, url string, payload, target any) error {
	s, err := c.authenticate(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if target == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}
