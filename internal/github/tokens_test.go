package github_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretEnv is a stand-in token used by the tests. It is deliberately fake and
// recognisable: if it ever shows up in an error message, a test asserting the
// opposite has caught a leak.
const secretEnv = "fake-token-value-must-never-be-echoed"

// writeEnvFile writes body to a .env file inside a fresh temp dir and returns
// both the directory and the file path. Every token test points
// LIGHTS_OUT_ENV_FILE at a temp dir so no test can read the repository's real
// .env.
func writeEnvFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// TestResolveTokenPrefersTheExplicitOverride covers D6 precedence: the explicit
// variable wins so a stray .env on a self-hosted runner cannot shadow the
// Actions-provided GITHUB_TOKEN.
func TestResolveTokenPrefersTheExplicitOverride(t *testing.T) {
	t.Setenv(github.TokenEnvVar, secretEnv)
	t.Setenv(github.FallbackTokenEnvVar, "fallback-should-not-win")
	t.Setenv(github.EnvFileEnvVar, writeEnvFile(t, "GITHUB_TOKEN=from-env-file\n"))

	token, source, err := github.ResolveToken()

	require.NoError(t, err)
	assert.Equal(t, secretEnv, token)
	assert.Equal(t, github.TokenEnvVar, source)
}

// TestResolveTokenFallsBackToGitHubToken proves the second source.
func TestResolveTokenFallsBackToGitHubToken(t *testing.T) {
	t.Setenv(github.TokenEnvVar, "")
	t.Setenv(github.FallbackTokenEnvVar, secretEnv)
	t.Setenv(github.EnvFileEnvVar, writeEnvFile(t, "GITHUB_TOKEN=from-env-file\n"))

	token, source, err := github.ResolveToken()

	require.NoError(t, err)
	assert.Equal(t, secretEnv, token)
	assert.Equal(t, github.FallbackTokenEnvVar, source)
}

// TestResolveTokenFallsBackToTheEnvFile proves the third source, and that a
// real environment variable still beats the file (D5).
func TestResolveTokenFallsBackToTheEnvFile(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{name: "GITHUB_TOKEN in the file", env: "GITHUB_TOKEN=" + secretEnv + "\n"},
		{name: "LIGHTS_OUT_GITHUB_TOKEN in the file", env: "LIGHTS_OUT_GITHUB_TOKEN=" + secretEnv + "\n"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := writeEnvFile(t, tt.env)
			t.Setenv(github.TokenEnvVar, "")
			t.Setenv(github.FallbackTokenEnvVar, "")
			t.Setenv(github.EnvFileEnvVar, path)

			token, source, err := github.ResolveToken()

			require.NoError(t, err)
			assert.Equal(t, secretEnv, token)
			assert.Equal(t, "env file "+path, source, "the source must name the file actually read")
		})
	}
}

// TestResolveTokenRejectsBlankValues guards against a whitespace-only variable
// becoming an "Authorization: Bearer    " header that authenticates as nobody.
func TestResolveTokenRejectsBlankValues(t *testing.T) {
	t.Setenv(github.TokenEnvVar, "   \t ")
	t.Setenv(github.FallbackTokenEnvVar, "\n")
	t.Setenv(github.EnvFileEnvVar, filepath.Join(t.TempDir(), "absent.env"))

	_, source, err := github.ResolveToken()

	require.Error(t, err)
	assert.Empty(t, source)
}

// TestResolveTokenIgnoresAnAbsentEnvFile proves a missing .env is not itself an
// error: the file is the lowest-precedence source, so its absence only matters
// when no variable supplied a token either.
func TestResolveTokenIgnoresAnAbsentEnvFile(t *testing.T) {
	t.Setenv(github.TokenEnvVar, "")
	t.Setenv(github.FallbackTokenEnvVar, secretEnv)
	t.Setenv(github.EnvFileEnvVar, filepath.Join(t.TempDir(), "absent.env"))

	token, _, err := github.ResolveToken()

	require.NoError(t, err)
	assert.Equal(t, secretEnv, token)
}

// TestResolveTokenMissingNamesEverySourceAndNoValue is the leak guard: the
// error has to tell an operator where to put the token, and it must not embed
// whatever was found along the way.
func TestResolveTokenMissingNamesEverySourceAndNoValue(t *testing.T) {
	cases := []struct {
		name    string
		envFile string
	}{
		{name: "no env file at all", envFile: ""},
		{name: "env file exists but holds no token", envFile: "SOMETHING_ELSE=1\n"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(github.TokenEnvVar, "")
			t.Setenv(github.FallbackTokenEnvVar, "")
			if tt.envFile == "" {
				t.Setenv(github.EnvFileEnvVar, filepath.Join(t.TempDir(), "absent.env"))
			} else {
				t.Setenv(github.EnvFileEnvVar, writeEnvFile(t, tt.envFile))
			}

			token, source, err := github.ResolveToken()

			require.Error(t, err)
			assert.Empty(t, token)
			assert.Empty(t, source)
			assert.ErrorContains(t, err, github.TokenEnvVar)
			assert.ErrorContains(t, err, github.FallbackTokenEnvVar)
			assert.ErrorContains(t, err, ".env")
		})
	}
}

// TestResolveTokenReportsAMalformedEnvFile proves a broken .env is surfaced
// rather than swallowed: the file exists precisely because someone tried to
// supply a token, so ignoring it would send them looking in the wrong place.
func TestResolveTokenReportsAMalformedEnvFile(t *testing.T) {
	path := writeEnvFile(t, "this line has no assignment\n")
	t.Setenv(github.TokenEnvVar, "")
	t.Setenv(github.FallbackTokenEnvVar, "")
	t.Setenv(github.EnvFileEnvVar, path)

	token, _, err := github.ResolveToken()

	require.Error(t, err)
	assert.Empty(t, token)
	assert.ErrorContains(t, err, path)
}

// TestResolveTokenReadsQuotedValuesFromTheEnvFile covers a .env written by hand
// with quotes around the secret, which is the most common .env mistake.
func TestResolveTokenReadsQuotedValuesFromTheEnvFile(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "double quoted", body: "GITHUB_TOKEN=\"" + secretEnv + "\"\n", want: secretEnv},
		{name: "single quoted", body: "GITHUB_TOKEN='" + secretEnv + "'\n", want: secretEnv},
		{name: "export prefixed", body: "export GITHUB_TOKEN=" + secretEnv + "\n", want: secretEnv},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(github.TokenEnvVar, "")
			t.Setenv(github.FallbackTokenEnvVar, "")
			t.Setenv(github.EnvFileEnvVar, writeEnvFile(t, tt.body))

			token, _, err := github.ResolveToken()

			require.NoError(t, err)
			assert.Equal(t, tt.want, token)
		})
	}
}

// TestResolveEnvFilePath covers the documented path override.
func TestResolveEnvFilePath(t *testing.T) {
	t.Run("env var wins over the default path", func(t *testing.T) {
		t.Setenv(github.EnvFileEnvVar, "/custom/secrets.env")
		assert.Equal(t, "/custom/secrets.env", github.ResolveEnvFilePath())
	})

	t.Run("falls back to the default when the env var is unset", func(t *testing.T) {
		t.Setenv(github.EnvFileEnvVar, "")
		assert.Equal(t, github.DefaultEnvFile, github.ResolveEnvFilePath())
	})

	t.Run("a blank env var falls back to the default", func(t *testing.T) {
		t.Setenv(github.EnvFileEnvVar, "   ")
		assert.Equal(t, github.DefaultEnvFile, github.ResolveEnvFilePath())
	})
}

// TestNewTokenHTTPClientSendsABearerHeader proves the credential reaches the
// server, and that it is the only authentication header present.
func TestNewTokenHTTPClientSendsABearerHeader(t *testing.T) {
	type received struct {
		authorization string
		token         string
	}
	got := make(chan received, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- received{authorization: r.Header.Get("Authorization"), token: r.Header.Get("X-Github-Token")}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := github.NewTokenHTTPClient(secretEnv).Do(req)

	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	r := <-got
	assert.Equal(t, "Bearer "+secretEnv, r.authorization)
	assert.Empty(t, r.token, "no other authentication header may be sent")
}

// TestNewTokenHTTPClientDoesNotMutateTheRequest keeps the transport honest: the
// http.RoundTripper contract forbids modifying the request it is given, and
// mutating it would leak the credential to any later retry or log of the
// original request.
func TestNewTokenHTTPClientDoesNotMutateTheRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := github.NewTokenHTTPClient(secretEnv).Do(req)

	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	assert.Empty(t, req.Header.Get("Authorization"), "the caller's request must not gain the credential")
}

// TestNewTokenHTTPClientPreservesTheRequestBody guards against a transport that
// satisfies authentication by dropping the caller's payload.
func TestNewTokenHTTPClientPreservesTheRequestBody(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		bodies <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"),
	)
	require.NoError(t, err)

	resp, err := github.NewTokenHTTPClient(secretEnv).Do(req)

	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	assert.Equal(t, "payload", <-bodies)
}
