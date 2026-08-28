package compute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
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
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
	Fault    *Fault            `json:"fault,omitempty"`
}

type Fault struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
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
	classIndex := map[string]int{"c1": 0, "c2": 1, "c3": 2}
	index, ok := classIndex[strings.ToLower(class)]
	if !ok || index >= len(shapes) {
		return Shape{}, "", nil, fmt.Errorf("unsupported compute class %q", class)
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
	s, err := c.authenticate(ctx)
	if err != nil {
		return Server{}, err
	}
	payload := map[string]any{"server": map[string]any{
		"name": name, "flavorRef": flavorID, "imageRef": imageID,
		"networks":  []map[string]string{{"uuid": networkID}},
		"metadata":  map[string]string{"canter.sandbox": sandbox, "canter.managed": "true"},
		"user_data": base64.StdEncoding.EncodeToString([]byte(userData)),
	}}
	var out struct {
		Server Server `json:"server"`
	}
	if err := c.request(ctx, http.MethodPost, s.ComputeURL+"/servers", payload, &out); err != nil {
		return Server{}, err
	}
	return out.Server, nil
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
		return fmt.Errorf("compute request failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if target == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}
