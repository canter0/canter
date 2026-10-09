package envfile

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/canter0/canter/internal/fileinput"
)

const maxEnvFileSize = 1 << 20

func Load() (string, error) {
	if explicit := os.Getenv("CANTER_ENV_FILE"); explicit != "" {
		return explicit, loadFile(explicit)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, ".env")
		if _, err := os.Stat(path); err == nil {
			return path, loadFile(path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("could not find .env in this directory or a parent; set CANTER_ENV_FILE")
}

func loadFile(path string) error {
	raw, err := fileinput.ReadRegular(path, maxEnvFileSize)
	if err != nil {
		return err
	}
	s := bufio.NewScanner(strings.NewReader(string(raw)))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		quoted := len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"'))
		singleQuoted := quoted && value[0] == '\''
		if quoted {
			value = value[1 : len(value)-1]
		}
		// Single-quoted values are literal. This preserves common credentials
		// containing dollar signs while preserving interpolation elsewhere.
		if !singleQuoted {
			value = os.ExpandEnv(value)
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return s.Err()
}
