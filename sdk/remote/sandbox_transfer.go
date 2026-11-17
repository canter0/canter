package remote

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/canter0/canter/sdk"
)

func (s *SandboxClient) TransferLimits(ctx context.Context, id string) (sdk.SandboxTransferLimits, error) {
	var out sdk.SandboxTransferLimits
	err := s.transferJSON(ctx, http.MethodGet, id, "", nil, &out)
	return out, err
}

func (s *SandboxClient) PlanTransfer(ctx context.Context, id string, input sdk.SandboxTransferRequest, dryRun bool) (sdk.SandboxTransfer, error) {
	var out sdk.SandboxTransfer
	err := s.transferJSON(ctx, http.MethodPost, id, "?dryRun="+strconv.FormatBool(dryRun), input, &out)
	return out, err
}

func (s *SandboxClient) Transfer(ctx context.Context, id, transfer string) (sdk.SandboxTransfer, error) {
	var out sdk.SandboxTransfer
	err := s.transferJSON(ctx, http.MethodGet, id, "/"+url.PathEscape(transfer), nil, &out)
	return out, err
}

func (s *SandboxClient) BindTransferDestination(ctx context.Context, id, transfer, binding string, versions []string) (sdk.SandboxTransfer, error) {
	var out sdk.SandboxTransfer
	err := s.transferJSON(ctx, http.MethodPut, id, "/"+url.PathEscape(transfer)+"/destination", map[string]any{"binding": binding, "versions": versions}, &out)
	return out, err
}

func (s *SandboxClient) CancelTransfer(ctx context.Context, id, transfer string) (sdk.SandboxTransfer, error) {
	var out sdk.SandboxTransfer
	err := s.transferJSON(ctx, http.MethodDelete, id, "/"+url.PathEscape(transfer), nil, &out)
	return out, err
}

func (s *SandboxClient) BeginTransferFile(ctx context.Context, id, transfer string, index int) (sdk.SandboxTransferFileReceipt, error) {
	var out sdk.SandboxTransferFileReceipt
	err := s.transferJSON(ctx, http.MethodPost, id, transferFilePath(transfer, index, "begin"), nil, &out)
	return out, err
}

func (s *SandboxClient) PutTransferChunk(ctx context.Context, id, transfer string, index int, offset int64, data []byte) (sdk.SandboxTransferFileReceipt, error) {
	var out sdk.SandboxTransferFileReceipt
	path, err := s.sandboxPath(id, "/transfers"+transferFilePath(transfer, index, "chunk")+"?offset="+strconv.FormatInt(offset, 10))
	if err != nil {
		return out, err
	}
	err = s.rawRequest(ctx, http.MethodPatch, path, bytes.NewReader(data), "application/octet-stream", &out)
	return out, err
}

type SandboxTransferChunk struct {
	Data       []byte `json:"data"`
	Size       int64  `json:"size"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	EOF        bool   `json:"eof"`
	Version    string `json:"version"`
}

func (s *SandboxClient) ReadTransferChunk(ctx context.Context, id, transfer string, index int, offset int64) (SandboxTransferChunk, error) {
	var out SandboxTransferChunk
	err := s.transferJSON(ctx, http.MethodGet, id, transferFilePath(transfer, index, "read")+"?offset="+strconv.FormatInt(offset, 10), nil, &out)
	return out, err
}

func (s *SandboxClient) CompleteTransferFile(ctx context.Context, id, transfer string, index int, sha string) (sdk.SandboxTransferFileReceipt, error) {
	var out sdk.SandboxTransferFileReceipt
	err := s.transferJSON(ctx, http.MethodPost, id, transferFilePath(transfer, index, "complete"), map[string]string{"sha256": sha}, &out)
	return out, err
}

func (s *SandboxClient) SkipTransferFile(ctx context.Context, id, transfer string, index int) (sdk.SandboxTransferFileReceipt, error) {
	var out sdk.SandboxTransferFileReceipt
	err := s.transferJSON(ctx, http.MethodPost, id, transferFilePath(transfer, index, "skip"), nil, &out)
	return out, err
}

func transferFilePath(transfer string, index int, action string) string {
	return fmt.Sprintf("/%s/%d/%s", url.PathEscape(transfer), index, action)
}

func (s *SandboxClient) transferJSON(ctx context.Context, method, id, suffix string, input, output any) error {
	path, err := s.sandboxPath(id, "/transfers"+suffix)
	if err != nil {
		return err
	}
	return s.client.do(ctx, method, path, s.accessToken, input, output)
}
