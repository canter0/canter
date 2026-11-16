package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const extensionMCPResponseLimit = 1 << 20

var errMCPUnauthorized = errors.New("the MCP server rejected this account; reconnect it before trying again")

func extensionMCPName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r)) {
			return false
		}
	}
	return true
}

// Each session is bound to one already-resolved endpoint and one caller's
// credential. Redirects, OAuth replays, standalone streams, sampling, roots and
// elicitation are deliberately not enabled by this tool client.
type extensionMCPTransport struct {
	base       http.RoundTripper
	endpoint   string
	credential string
	status     atomic.Int32
}

func (t *extensionMCPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.String() != t.endpoint {
		return nil, fmt.Errorf("MCP destination changed")
	}
	copy := request.Clone(request.Context())
	copy.GetBody = nil
	copy.Header.Del("Authorization")
	if t.credential != "" {
		copy.Header.Set("Authorization", "Bearer "+t.credential)
	}
	copy.Header.Set("User-Agent", "Canter-Connectors/1.0 (+https://canter.dev)")
	response, err := t.base.RoundTrip(copy)
	copy.Header.Del("Authorization")
	if err != nil {
		return nil, err
	}
	t.status.Store(int32(response.StatusCode))
	response.Body = &extensionMCPBody{ReadCloser: response.Body, remaining: extensionMCPResponseLimit}
	return response, nil
}

type extensionMCPBody struct {
	io.ReadCloser
	remaining int
}

func (b *extensionMCPBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("MCP response exceeds limit")
		}
		return 0, err
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}
func extensionMCPSession(ctx context.Context, client *http.Client, endpoint, credential string) (*mcp.ClientSession, *extensionMCPTransport, error) {
	transport := &extensionMCPTransport{base: client.Transport, endpoint: endpoint, credential: credential}
	isolated := *client
	isolated.Transport = transport
	isolated.Timeout = 40 * time.Second
	c := mcp.NewClient(&mcp.Implementation{Name: "Canter", Version: "1.0"}, &mcp.ClientOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	session, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &isolated, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: extensionMCPResponseLimit}, nil)
	return session, transport, err
}

func performWrenMCP(ctx context.Context, client *http.Client, tool WrenExtensionTool, args json.RawMessage, credential string) (json.RawMessage, int, string, string) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	session, transport, err := extensionMCPSession(ctx, client, tool.Endpoint, credential)
	if err != nil {
		return nil, int(transport.status.Load()), "failed", "Couldn’t connect to this MCP server. Check the endpoint and your account access."
	}
	defer session.Close()
	name := tool.RemoteName
	if name == "" {
		name = tool.Name
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	status := int(transport.status.Load())
	if err != nil {
		if tool.Impact == "read" {
			return nil, status, "failed", "The MCP server could not return a usable result. Try again later."
		}
		return nil, status, "unknown", "The MCP request may have been processed. No blind retry was sent. Check the service before trying again."
	}
	if result.IsError {
		return nil, status, "failed", "The MCP server reported that the tool failed. Check the request and account access."
	}
	if result.NeedsInput() || len(result.InputRequests) != 0 {
		return nil, status, mcpUncertainStatus(tool), "This MCP tool requires an additional interaction that this connection does not support. Check the service before repeating a change."
	}
	// Do not relay server requests for client input or execute embedded UI. This
	// integration supports completed text/structured tool results only.
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > 256<<10 {
		return nil, status, mcpUncertainStatus(tool), "The MCP result exceeded the supported size. Check the service before repeating a change."
	}
	var document any
	if err = json.Unmarshal(raw, &document); err != nil {
		return nil, status, mcpUncertainStatus(tool), "The MCP result could not be read."
	}
	if len(tool.OutputSchema) > 0 {
		schema, schemaErr := extensionSchema(tool.OutputSchema, false)
		if schemaErr != nil || schema.Validate(result.StructuredContent) != nil {
			return nil, status, mcpUncertainStatus(tool), "The MCP result did not match the published output schema."
		}
	}
	safe, _ := json.Marshal(extensionRedactJSON(document, credential))
	return safe, status, "completed", ""
}
func mcpUncertainStatus(tool WrenExtensionTool) string {
	if tool.Impact == "read" {
		return "failed"
	}
	return "unknown"
}

type wrenMCPDiscovery struct {
	Name     string              `json:"name"`
	Origin   string              `json:"origin"`
	Endpoint string              `json:"endpoint"`
	Auth     string              `json:"auth"`
	Tools    []WrenExtensionTool `json:"tools"`
	Warnings []string            `json:"warnings"`
}

func discoverExtensionMCP(ctx context.Context, config WrenExtensionConfig, endpoint, credential string) (wrenMCPDiscovery, error) {
	out := wrenMCPDiscovery{Tools: []WrenExtensionTool{}, Warnings: []string{}, Auth: "none"}
	u, _, err := extensionURL(endpoint, config)
	if err != nil {
		return out, err
	}
	if credential != "" {
		if len(credential) < 8 || len(credential) > 4096 || strings.IndexFunc(credential, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return out, fmt.Errorf("provide a valid bearer token")
		}
		out.Auth = "bearer"
	}
	out.Endpoint, out.Origin, out.Name = u.String(), u.Scheme+"://"+strings.ToLower(u.Host), u.Hostname()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client, closeClient, err := extensionHTTPClient(ctx, config, out.Endpoint)
	if err != nil {
		return out, err
	}
	defer closeClient()
	session, transport, err := extensionMCPSession(ctx, client, out.Endpoint, credential)
	if err != nil {
		if transport.status.Load() == http.StatusUnauthorized {
			return out, errMCPUnauthorized
		}
		return out, fmt.Errorf("couldn’t connect to this MCP server; check its URL and whether it requires a bearer token or OAuth")
	}
	defer session.Close()
	seenNames, seenCursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	count := 0
	for page := 0; page < 8; page++ {
		list, listErr := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if listErr != nil {
			if transport.status.Load() == http.StatusUnauthorized {
				return out, errMCPUnauthorized
			}
			return out, fmt.Errorf("the MCP server could not list its tools")
		}
		for _, remote := range list.Tools {
			count++
			if count > 64 {
				return out, fmt.Errorf("this server exposes more than 64 tools; use a scoped server endpoint")
			}
			if remote == nil || !extensionMCPName(remote.Name) || seenNames[remote.Name] {
				return out, fmt.Errorf("the MCP server returned invalid or duplicate tool names")
			}
			seenNames[remote.Name] = true
			name := remote.Name
			if !extensionName(name) {
				digest := sha256.Sum256([]byte(name))
				name = "tool_" + hex.EncodeToString(digest[:10])
			}
			schema, _ := json.Marshal(remote.InputSchema)
			output, _ := json.Marshal(remote.OutputSchema)
			if bytes.Equal(output, []byte("null")) {
				output = nil
			}
			description := strings.TrimSpace(remote.Description)
			if description == "" {
				description = "Use " + remote.Name + " on " + u.Hostname() + "."
			}
			tool := WrenExtensionTool{Name: name, RemoteName: remote.Name, Method: "MCP", Description: description, Endpoint: out.Endpoint, Impact: "write", InputSchema: schema, OutputSchema: output}
			input := WrenExtensionInput{ID: "extension_mcp_discovery", Manifest: WrenExtensionManifest{Name: out.Name, Origin: out.Origin, Auth: out.Auth, Tools: []WrenExtensionTool{tool}}}
			if validateExtension(&input, config) != nil {
				out.Warnings = append(out.Warnings, remote.Name+": unsupported description or schema; not available to add.")
				continue
			}
			// Provider annotations are hints, not authorization. The human reviews
			// impact and selects tools before publishing and granting conversation use.
			out.Tools = append(out.Tools, input.Manifest.Tools[0])
		}
		if list.NextCursor == "" {
			return redactMCPDiscovery(out, credential), nil
		}
		if len(list.NextCursor) > 4096 || seenCursors[list.NextCursor] {
			return out, fmt.Errorf("the MCP server returned invalid pagination")
		}
		seenCursors[list.NextCursor] = true
		cursor = list.NextCursor
	}
	return out, fmt.Errorf("the MCP server exceeded the discovery page limit")
}
func (h *HTTPServer) discoverWrenMCP(w http.ResponseWriter, r *http.Request, workspace, account string) {
	if err := h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, account); err != nil {
		writeStoreError(w, err)
		return
	}
	var input struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if !decodeLimit(w, r, &input, 8<<10) {
		return
	}
	result, err := discoverExtensionMCP(r.Context(), h.config.Extensions, strings.TrimSpace(input.Endpoint), input.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// A provider can echo its Authorization value in tool descriptions or schemas.
// Discovery metadata must not turn a write-only credential into a UI response.
func redactMCPDiscovery(value wrenMCPDiscovery, credential string) wrenMCPDiscovery {
	raw, _ := json.Marshal(value)
	var document any
	_ = json.Unmarshal(raw, &document)
	raw, _ = json.Marshal(redactMCPMetadata(document, credential))
	var safe wrenMCPDiscovery
	_ = json.Unmarshal(raw, &safe)
	return safe
}

// Schema property names such as "password" are definitions, not stored
// credentials. Keep those definitions intact while removing echoed token values.
func redactMCPMetadata(value any, credential string) any {
	if credential == "" {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[strings.ReplaceAll(key, credential, "[redacted]")] = redactMCPMetadata(child, credential)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = redactMCPMetadata(child, credential)
		}
		return out
	case string:
		return strings.ReplaceAll(v, credential, "[redacted]")
	default:
		return value
	}
}
