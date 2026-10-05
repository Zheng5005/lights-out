package github

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

const (
	// TokenEnvVar is the explicit token override. It is checked first so a
	// stray .env file on a self-hosted runner can never shadow the
	// Actions-provided GITHUB_TOKEN.
	TokenEnvVar = "LIGHTS_OUT_GITHUB_TOKEN"

	// FallbackTokenEnvVar is the variable GitHub Actions injects, and the key
	// also honoured inside the .env file.
	FallbackTokenEnvVar = "GITHUB_TOKEN"

	// EnvFileEnvVar overrides the path of the .env file consulted last.
	EnvFileEnvVar = "LIGHTS_OUT_ENV_FILE"

	// DefaultEnvFile is the .env path used when EnvFileEnvVar is unset.
	DefaultEnvFile = ".env"
)

// tokenSources lists the variable names carrying a token, in precedence order.
// The .env file is consulted after both of them and honours the same names, so
// a file-based token is exactly as explicit as an environment one.
var tokenSources = []string{TokenEnvVar, FallbackTokenEnvVar}

// ResolveToken returns the GitHub token and the name of the source that
// provided it.
//
// The source is returned alongside the value so callers can report where the
// credential came from without ever echoing it. A missing token is an error
// naming every source that was tried.
//
// Precedence is fixed: TokenEnvVar, then FallbackTokenEnvVar, then the .env
// file. Real environment variables always beat the file, so a runner-provided
// token wins over anything checked into a working directory.
func ResolveToken() (token, source string, err error) {
	for _, name := range tokenSources {
		if value := readEnvVar(name); value != "" {
			return value, name, nil
		}
	}

	path := ResolveEnvFilePath()

	values, err := readEnvFile(path)
	if err != nil {
		return "", "", err
	}
	for _, name := range tokenSources {
		if value := strings.TrimSpace(values[name]); value != "" {
			return value, fmt.Sprintf("env file %s", path), nil
		}
	}

	// Names the sources, never the values: by construction no value was found,
	// but the message stays value-free so it is always safe to print.
	return "", "", fmt.Errorf(
		"no GitHub token found: set %s, set %s, or add %s to the %s file (looked in %q)",
		TokenEnvVar, FallbackTokenEnvVar, FallbackTokenEnvVar, DefaultEnvFile, path,
	)
}

// ResolveEnvFilePath returns the .env path: the value of EnvFileEnvVar when it
// is set and non-empty, otherwise DefaultEnvFile.
func ResolveEnvFilePath() string {
	if fromEnv := strings.TrimSpace(os.Getenv(EnvFileEnvVar)); fromEnv != "" {
		return fromEnv
	}
	return DefaultEnvFile
}

// readEnvVar returns a trimmed environment value. Trimming matters: a variable
// set to whitespace would otherwise authenticate as an empty credential.
func readEnvVar(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// readEnvFile parses path into a key/value map.
//
// A missing file is not an error: the file is the lowest-precedence source, so
// its absence only matters when no variable supplied a token either, and
// ResolveToken reports that case with a better message. A file that exists but
// cannot be parsed IS an error, because silently ignoring it would hide a typo
// in the very file an operator created to fix a missing token.
//
// godotenv.Read is used rather than godotenv.Load on purpose: Read returns the
// values without writing them into the process environment, so resolving a
// token has no side effect on unrelated code sharing the process.
func readEnvFile(path string) (map[string]string, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read env file %q: %w", path, err)
	}
	return values, nil
}

// NewTokenHTTPClient returns an http.Client that authenticates every request
// with token as a bearer credential.
//
// This is the entire authentication surface of Phase 2. There is deliberately
// no oauth2 dependency: oauth2 earns its keep in the three-legged browser flow,
// which a cron factory never runs, and dropping it removes a dependency whose
// go directive could otherwise raise the module's Go version ceiling.
func NewTokenHTTPClient(token string) *http.Client {
	return &http.Client{
		Transport: &bearerTransport{token: token, base: http.DefaultTransport},
	}
}

// bearerTransport injects the Authorization header. It is unexported because
// the contract is "an http.Client that authenticates"; the transport itself is
// an implementation detail with no caller-visible behaviour to depend on.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip clones the request before setting the credential. The
// http.RoundTripper contract forbids modifying the request it is handed, and
// the reason is security rather than tidiness: a mutated request would carry
// the token into any later retry, redirect log, or recorded request dump.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	authorized := req.Clone(req.Context())
	authorized.Header.Set("Authorization", "Bearer "+t.token)
	return t.baseTransport().RoundTrip(authorized)
}

// baseTransport falls back to the default transport so a zero-value
// bearerTransport is still usable instead of panicking on a nil inner round trip.
func (t *bearerTransport) baseTransport() http.RoundTripper {
	if t.base != nil {
		return t.base
	}
	return http.DefaultTransport
}
