//go:build integration

package integration

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const storeVersion = "0.1.0"

type storeRegistry struct {
	Plugins []struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
	} `json:"plugins"`
}

type storeHost struct {
	*hostProcess
	root      string
	config    string
	pluginDir string
	upstream  string
	registry  string
}

func TestStoreInstall(t *testing.T) {
	if os.Getenv("STORE_INSTALL") != "1" {
		t.Skip("нужен доступ к GitHub")
	}
	if os.Getenv("CLIPROXY_BIN") == "" {
		t.Skip("нужен CLIPROXY_BIN")
	}

	registryURL, repository := storeRegistryURL(t)
	archivePath := filepath.Join(t.TempDir(), "gigacode.zip")
	if err := downloadStoreArchive(t, archivePath); err != nil {
		t.Fatal(err)
	}
	wantBytes := storeArchiveFile(t, archivePath)
	wantHash := sha256.Sum256(wantBytes)
	wantSHA := hex.EncodeToString(wantHash[:])

	state := newMockState()
	mock := newMock(t, state)
	defer mock.Close()
	host := startStoreHost(t, mock.URL, registryURL)
	installedPath := filepath.Join(host.pluginDir, "linux", "amd64", "gigacode-v"+storeVersion+".so")
	installed := false
	t.Run("реестр", func(t *testing.T) {
		response := request(t, http.MethodGet, host.base+"/v0/management/plugin-store?refresh=true", managementKey, nil)
		if retryAfter, limited := storeRateLimit(t, response); limited {
			t.Skipf("лимит GitHub, повторить после %s", retryAfter)
		}
		var catalog struct {
			Plugins []struct {
				ID, Version, Repository string `json:",omitempty"`
			} `json:"plugins"`
		}
		readJSON(t, response, &catalog)
		for _, plugin := range catalog.Plugins {
			if plugin.ID == "gigacode" {
				if plugin.Repository != repository {
					t.Fatalf("catalog plugin=%+v", plugin)
				}
				t.Logf("Store catalog: id=%s repository=%s", plugin.ID, plugin.Repository)
				return
			}
		}
		t.Fatal("catalog does not contain gigacode")
	})
	t.Run("установка", func(t *testing.T) {
		response := request(t, http.MethodPost, host.base+"/v0/management/plugin-store/gigacode/install", managementKey, []byte(`{"version":"0.1.0"}`))
		if retryAfter, limited := storeRateLimit(t, response); limited {
			t.Skipf("лимит GitHub, повторить после %s", retryAfter)
		}
		var install map[string]any
		readJSON(t, response, &install)
		if response.StatusCode != http.StatusOK || install["status"] != "installed" || install["version"] != storeVersion || install["restart_required"] != false {
			t.Fatalf("install status=%d response=%s", response.StatusCode, compactJSON(install))
		}
		installed = true
	})
	t.Run("файл-и-контрольная-сумма", func(t *testing.T) {
		if !installed {
			t.Skip("установка магазина пропущена")
		}
		installedBytes, err := os.ReadFile(installedPath)
		if err != nil {
			t.Fatal(err)
		}
		gotHash := sha256.Sum256(installedBytes)
		if !bytes.Equal(installedBytes, wantBytes) || hex.EncodeToString(gotHash[:]) != wantSHA {
			t.Fatalf("installed file differs: size=%d sha256=%s", len(installedBytes), hex.EncodeToString(gotHash[:]))
		}
		t.Logf("Installed file: path=%s size=%d sha256=%s", installedPath, len(installedBytes), hex.EncodeToString(gotHash[:]))
	})
	t.Run("регистрация-и-работа", func(t *testing.T) {
		if !installed {
			t.Skip("установка магазина пропущена")
		}
		checkInstalledPlugin(t, host, storeVersion)
		checkStorePluginTraffic(t, host, state)
	})
	t.Run("перезапуск", func(t *testing.T) {
		if !installed {
			prepareStoreRestart(t, host, wantBytes)
		}
		host.stop()
		host = startStoreHostWithRoot(t, host.root, host.config, host.pluginDir, host.upstream, host.registry)
		checkRegisteredPlugin(t, host, storeVersion)
		checkStorePluginTraffic(t, host, state)
	})
	t.Run("повторная-установка", func(t *testing.T) {
		if !installed {
			t.Skip("установка магазина пропущена")
		}
		before, err := os.ReadFile(installedPath)
		if err != nil {
			t.Fatal(err)
		}
		response := request(t, http.MethodPost, host.base+"/v0/management/plugin-store/gigacode/install", managementKey, []byte(`{"version":"0.1.0"}`))
		if retryAfter, limited := storeRateLimit(t, response); limited {
			t.Skipf("лимит GitHub, повторить после %s", retryAfter)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		var repeat map[string]any
		json.Unmarshal(body, &repeat)
		if response.StatusCode != http.StatusOK || repeat["status"] != "installed" || repeat["version"] != storeVersion || repeat["restart_required"] != false {
			t.Fatalf("repeat install: status=%d body=%s", response.StatusCode, body)
		}
		after, err := os.ReadFile(installedPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("repeat install changed file")
		}
		checkRegisteredPlugin(t, host, storeVersion)
	})
}

func storeRegistryURL(t *testing.T) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry storeRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range registry.Plugins {
		if plugin.ID == "gigacode" {
			repository := strings.TrimSuffix(plugin.Repository, "/")
			parts := strings.Split(strings.TrimPrefix(repository, "https://github.com/"), "/")
			if len(parts) != 2 {
				t.Fatalf("unexpected repository URL %q", plugin.Repository)
			}
			return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/registry.json", parts[0], parts[1]), repository
		}
	}
	t.Fatal("gigacode absent from registry.json")
	return "", ""
}

func downloadStoreArchive(t *testing.T, path string) error {
	t.Helper()
	cmd := exec.Command("gh", "release", "download", "v"+storeVersion, "-R", "spa-skyson/gigacode-cliproxy-plugin", "-p", "gigacode_0.1.0_linux_amd64.zip", "-D", filepath.Dir(path), "--clobber")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("download release: %w: %s", err, output)
	}
	return os.Rename(filepath.Join(filepath.Dir(path), "gigacode_0.1.0_linux_amd64.zip"), path)
}

func storeArchiveFile(t *testing.T, path string) []byte {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if file.Name != "gigacode.so" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	t.Fatal("gigacode.so absent from release archive")
	return nil
}

func startStoreHost(t *testing.T, upstream, registryURL string) *storeHost {
	t.Helper()
	root := t.TempDir()
	return startStoreHostWithRoot(t, root, filepath.Join(root, "config.yaml"), filepath.Join(root, "plugins"), upstream, registryURL)
}

func startStoreHostWithRoot(t *testing.T, root, configPath, pluginDir string, args ...string) *storeHost {
	t.Helper()
	upstream, registryURL := args[0], args[1]
	if err := os.MkdirAll(filepath.Join(root, "auths"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(root, "auths", "gigacode.json")
	if _, err := os.Stat(authPath); os.IsNotExist(err) {
		if err := os.WriteFile(authPath, []byte(`{"type":"gigacode","device_code":"`+deviceCode+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	port := freePort(t)
	config := fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
  response-compression:
    enabled: false
management:
  allow-remote: false
  secret-key: "%s"
  disable-control-panel: true
access:
  api-keys:
    - "%s"
oauth:
  auth-dir: %q
plugins:
  enabled: true
  dir: %q
  store-sources:
    - %q
  configs:
    gigacode:
      enabled: false
      base-url: %q
      refresh-margin-seconds: 2
      auth-timeout-seconds: 3
      retry-seconds: 1
`, port, managementKey, clientKey, filepath.Join(root, "auths"), pluginDir, registryURL, upstream+"/api/v2")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		current, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(current), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "port:") {
				lines[i] = fmt.Sprintf("  port: %d", port)
				break
			}
		}
		if err := os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stderrPath := filepath.Join(root, "stderr.log")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Getenv("CLIPROXY_BIN"), "--config", configPath, "--local-model")
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	host := &storeHost{hostProcess: &hostProcess{base: fmt.Sprintf("http://127.0.0.1:%d", port), stderr: stderrPath, cmd: cmd}, root: root, config: configPath, pluginDir: pluginDir, upstream: upstream, registry: registryURL}
	host.stopFunc = func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait(); _ = stderr.Close() }
	t.Cleanup(host.stop)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := http.Get(host.base + "/healthz")
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return host
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("хост не готов: %s", readFile(stderrPath))
	return nil
}

func checkInstalledPlugin(t *testing.T, host *storeHost, version string) {
	t.Helper()
	var entry pluginEntry
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response := request(t, http.MethodGet, host.base+"/v0/management/plugins", managementKey, nil)
		var list pluginList
		readJSON(t, response, &list)
		entry = findPlugin(t, list)
		if entry.Registered && entry.EffectiveEnabled {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("Management plugin: id=%s registered=%v enabled=%v", entry.ID, entry.Registered, entry.EffectiveEnabled)
	if !entry.Registered || !entry.EffectiveEnabled {
		t.Fatalf("plugin status: %+v", entry)
	}
	response := request(t, http.MethodGet, host.base+"/v0/management/plugin-store", managementKey, nil)
	var catalog struct {
		Plugins []struct {
			ID               string `json:"id"`
			InstalledVersion string `json:"installed_version"`
		} `json:"plugins"`
	}
	readJSON(t, response, &catalog)
	for _, plugin := range catalog.Plugins {
		if plugin.ID == "gigacode" && plugin.InstalledVersion != version {
			t.Fatalf("installed version=%q", plugin.InstalledVersion)
		}
	}
}

func checkStorePluginTraffic(t *testing.T, host *storeHost, state *mockState) {
	t.Helper()
	if !waitForModel(t, host.hostProcess, "vllm/CodeAgent", 15*time.Second) {
		t.Fatal("store plugin model absent")
	}
	response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[],"stream":false}`))
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("mock response")) {
		t.Fatalf("non-stream chat: status=%d body=%s", response.StatusCode, body)
	}
	state.mu.Lock()
	state.mode = "stream"
	state.mu.Unlock()
	response = request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[],"stream":true}`))
	stream, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(stream, []byte("one")) || !bytes.Contains(stream, []byte("two")) {
		t.Fatalf("stream chat: status=%d body=%s", response.StatusCode, stream)
	}
	state.mu.Lock()
	state.mode = ""
	state.mu.Unlock()
	t.Logf("Chat results: non-stream=mock response stream=one two")
}

func compactJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func storeRateLimit(t *testing.T, response *http.Response) (string, bool) {
	t.Helper()
	if response.StatusCode != http.StatusTooManyRequests {
		return "", false
	}
	var raw struct {
		Error      string      `json:"error"`
		RetryAfter json.Number `json:"retry_after"`
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if err := json.Unmarshal(data, &raw); err != nil || raw.Error != "plugin_store_rate_limited" {
		t.Fatalf("unexpected 429 from plugin store: %s", data)
	}
	return raw.RetryAfter.String(), true
}

func checkRegisteredPlugin(t *testing.T, host *storeHost, version string) {
	response := request(t, http.MethodGet, host.base+"/v0/management/plugins", managementKey, nil)
	var list struct {
		Plugins []struct {
			ID       string `json:"id"`
			Metadata struct {
				Version string `json:"version"`
			} `json:"metadata"`
			Registered bool `json:"registered"`
			Enabled    bool `json:"effective_enabled"`
		} `json:"plugins"`
	}
	readJSON(t, response, &list)
	var entry struct {
		ID         string
		Version    string
		Registered bool
		Enabled    bool
	}
	for _, item := range list.Plugins {
		if item.ID == "gigacode" {
			entry.ID, entry.Version, entry.Registered, entry.Enabled = item.ID, item.Metadata.Version, item.Registered, item.Enabled
		}
	}
	if !entry.Registered || !entry.Enabled || entry.Version != version {
		t.Fatalf("plugin status: %+v", entry)
	}
}

func prepareStoreRestart(t *testing.T, host *storeHost, pluginBytes []byte) {
	t.Helper()
	path := filepath.Join(host.pluginDir, "linux", "amd64", "gigacode-v"+storeVersion+".so")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pluginBytes, 0755); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(host.config)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config, []byte("      enabled: false"), []byte("      enabled: true"), 1)
	if err := os.WriteFile(host.config, config, 0600); err != nil {
		t.Fatal(err)
	}
}
