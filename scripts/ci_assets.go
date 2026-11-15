package ciassets

import "embed"

//go:embed canter-ci-runner.py canter-ci-jit-runner.py canter-ci-admission-client.py canter-ci-guest-bootstrap.py canter-ci-machine-init.py canter-ci-cache.py canter-ci-evidence.py
var sources embed.FS

var names = [...]string{"canter-ci-runner.py", "canter-ci-jit-runner.py", "canter-ci-admission-client.py", "canter-ci-guest-bootstrap.py", "canter-ci-machine-init.py", "canter-ci-cache.py", "canter-ci-evidence.py"}

// Sources returns fresh copies of the sealed guest tools and boot template.
func Sources() (map[string][]byte, error) {
	out := make(map[string][]byte, len(names))
	for _, name := range names {
		raw, err := sources.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out[name] = raw
	}
	return out, nil
}
