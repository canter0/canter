package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const ciAdmissionRequestMaxBytes = 16384

var errCIAdmissionRequestInvalid = errors.New("invalid CI admission request")

var ciAdmissionRequestFields = map[string]bool{
	"schemaVersion":      true,
	"admissionId":        true,
	"githubRunnerId":     true,
	"githubRepositoryId": true,
	"repository":         true,
	"githubRunId":        true,
	"runAttempt":         true,
	"jobKey":             true,
	"runnerName":         true,
}

// decodeCIAdmissionRequest parses the exact nine-field admission protocol
// object. It rejects oversized bodies, non-objects, duplicate or unknown
// keys, missing keys, trailing data, nulls, and mistyped or out-of-range
// numbers before the typed request contract is applied.
func decodeCIAdmissionRequest(raw []byte) (ciAdmissionRequest, error) {
	var req ciAdmissionRequest
	if len(raw) == 0 || len(raw) > ciAdmissionRequestMaxBytes {
		return req, errCIAdmissionRequestInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return req, errCIAdmissionRequestInvalid
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return req, errCIAdmissionRequestInvalid
	}
	values := make(map[string]json.RawMessage, len(ciAdmissionRequestFields))
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return req, errCIAdmissionRequestInvalid
		}
		key, ok := keyTok.(string)
		if !ok || !ciAdmissionRequestFields[key] {
			return req, errCIAdmissionRequestInvalid
		}
		if _, dup := values[key]; dup {
			return req, errCIAdmissionRequestInvalid
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return req, errCIAdmissionRequestInvalid
		}
		values[key] = value
	}
	if tok, err = dec.Token(); err != nil {
		return req, errCIAdmissionRequestInvalid
	} else if d, ok := tok.(json.Delim); !ok || d != '}' {
		return req, errCIAdmissionRequestInvalid
	}
	if _, err = dec.Token(); err != io.EOF {
		return req, errCIAdmissionRequestInvalid
	}
	if len(values) != len(ciAdmissionRequestFields) {
		return req, errCIAdmissionRequestInvalid
	}
	stringField := func(key string, dst *string) error {
		value := bytes.TrimSpace(values[key])
		if bytes.Equal(value, []byte("null")) {
			return errCIAdmissionRequestInvalid
		}
		if err := json.Unmarshal(value, dst); err != nil {
			return errCIAdmissionRequestInvalid
		}
		return nil
	}
	intField := func(key string, dst *int64) error {
		value := bytes.TrimSpace(values[key])
		if bytes.Equal(value, []byte("null")) {
			return errCIAdmissionRequestInvalid
		}
		var n int64
		if err := json.Unmarshal(value, &n); err != nil {
			return errCIAdmissionRequestInvalid
		}
		*dst = n
		return nil
	}
	var schemaVersion int64
	if err := intField("schemaVersion", &schemaVersion); err != nil {
		return req, err
	}
	if schemaVersion != 1 {
		return req, errCIAdmissionRequestInvalid
	}
	req.SchemaVersion = int(schemaVersion)
	if err := stringField("admissionId", &req.AdmissionID); err != nil {
		return req, err
	}
	if err := intField("githubRunnerId", &req.GitHubRunnerID); err != nil {
		return req, err
	}
	if err := intField("githubRepositoryId", &req.GitHubRepositoryID); err != nil {
		return req, err
	}
	if err := stringField("repository", &req.Repository); err != nil {
		return req, err
	}
	if err := intField("githubRunId", &req.GitHubRunID); err != nil {
		return req, err
	}
	if err := intField("runAttempt", &req.RunAttempt); err != nil {
		return req, err
	}
	if err := stringField("jobKey", &req.JobKey); err != nil {
		return req, err
	}
	if err := stringField("runnerName", &req.RunnerName); err != nil {
		return req, err
	}
	if !req.valid() {
		return ciAdmissionRequest{}, errCIAdmissionRequestInvalid
	}
	return req, nil
}
