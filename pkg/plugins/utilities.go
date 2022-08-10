package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/BurntSushi/toml"

	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/ansi"
	"github.com/stripe/stripe-cli/pkg/config"
	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

// GetBinaryExtension returns the appropriate file extension for plugin binary
func GetBinaryExtension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}

	return ""
}

// getPluginsDir computes where plugins are installed locally
func getPluginsDir(config config.IConfig) string {
	var pluginsDir string
	tempEnvPluginsPath := os.Getenv("STRIPE_PLUGINS_PATH")

	switch {
	case tempEnvPluginsPath != "":
		pluginsDir = tempEnvPluginsPath
	case PluginsPath != "":
		pluginsDir = PluginsPath
	default:
		configPath := config.GetConfigFolder(os.Getenv("XDG_CONFIG_HOME"))
		pluginsDir = filepath.Join(configPath, "plugins")
	}

	return pluginsDir
}

// GetPluginList builds a list of allowed plugins to be installed and run by the CLI
func GetPluginList(ctx context.Context, config config.IConfig, fs afero.Fs) (PluginList, error) {
	var pluginList PluginList
	configPath := config.GetConfigFolder(os.Getenv("XDG_CONFIG_HOME"))
	pluginManifestPath := filepath.Join(configPath, "plugins.toml")

	file, err := afero.ReadFile(fs, pluginManifestPath)
	if os.IsNotExist(err) {
		log.Debug("The plugin manifest file does not exist. Downloading...")
		err = RefreshPluginManifest(ctx, config, fs, stripe.DefaultAPIBaseURL)
		if err != nil {
			log.Debug("Could not download plugin manifest")
			return pluginList, err
		}
		file, err = afero.ReadFile(fs, pluginManifestPath)
	}

	if err != nil {
		return pluginList, err
	}

	_, err = toml.Decode(string(file), &pluginList)
	if err != nil {
		return pluginList, err
	}

	return pluginList, nil
}

// LookUpPlugin returns the matching plugin object
func LookUpPlugin(ctx context.Context, config config.IConfig, fs afero.Fs, pluginName string) (Plugin, error) {
	var plugin Plugin
	pluginList, err := GetPluginList(ctx, config, fs)
	if err != nil {
		return plugin, err
	}

	for _, p := range pluginList.Plugins {
		if pluginName == p.Shortname {
			return p, nil
		}
	}

	return plugin, fmt.Errorf("Could not find a plugin named %s", pluginName)
}

// RefreshPluginManifest refreshes the plugin manifest
func RefreshPluginManifest(ctx context.Context, config config.IConfig, fs afero.Fs, baseURL string) error {
	apiKey, err := config.GetProfile().GetAPIKey(false)
	if err != nil {
		return err
	}

	pluginData, err := requests.GetPluginData(ctx, baseURL, stripe.APIVersion, apiKey, config.GetProfile())
	if err != nil {
		return err
	}

	pluginManifestURL := fmt.Sprintf("%s/%s", pluginData.PluginBaseURL, "plugins.toml")
	body, err := FetchRemoteResource(pluginManifestURL)
	if err != nil {
		return err
	}

	configPath := config.GetConfigFolder(os.Getenv("XDG_CONFIG_HOME"))
	pluginManifestPath := filepath.Join(configPath, "plugins.toml")

	err = afero.WriteFile(fs, pluginManifestPath, body, 0644)

	if err != nil {
		return err
	}

	return nil
}

// AddEntryToPluginManifest update plugins.toml with a new release version
func AddEntryToPluginManifest(entry Plugin, config config.IConfig) error {
	configPath := config.GetConfigFolder(os.Getenv("XDG_CONFIG_HOME"))
	pluginManifestPath := filepath.Join(configPath, "plugins.toml")

	var currentPluginList PluginList
	if _, err := os.Stat(pluginManifestPath); err == nil {
		if _, err := toml.DecodeFile(pluginManifestPath, &currentPluginList); err != nil {
			return err
		}
	}

	foundPlugin := false
	for i, plugin := range currentPluginList.Plugins {
		// already a plugin in the manfest with the same name, so use this instead of making a new one
		if plugin.Shortname == entry.Shortname {
			// plugin already installed. append a new release version
			foundPlugin = true
			currentPluginList.Plugins[i].Releases = append(currentPluginList.Plugins[i].Releases, entry.Releases[0])
			break
		}
	}

	if !foundPlugin {
		// plugin does not exist. add a new plugin with a new dev release
		currentPluginList.Plugins = append(currentPluginList.Plugins, entry)
	}

	buf := new(bytes.Buffer)
	err := toml.NewEncoder(buf).Encode(currentPluginList)
	if err != nil {
		return err
	}

	err = os.WriteFile(pluginManifestPath, buf.Bytes(), 0644)
	if err != nil {
		return err
	}

	config.InitConfig()
	installedList := config.GetInstalledPlugins()

	// check for plugin already in list (ie. in the case of an upgrade)
	isInstalled := false
	for _, name := range installedList {
		if name == entry.Shortname {
			isInstalled = true
		}
	}

	if !isInstalled {
		installedList = append(installedList, entry.Shortname)
	}

	// sync list of installed plugins to file
	config.WriteConfigField("installed_plugins", installedList)

	return nil
}

// FetchRemoteResource returns the remote resource body
func FetchRemoteResource(url string) ([]byte, error) {
	t := &requests.TracedTransport{}

	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		return nil, err
	}

	trace := &httptrace.ClientTrace{
		GotConn: t.GotConn,
		DNSDone: t.DNSDone,
	}

	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	client := &http.Client{Transport: t}

	resp, err := client.Do(req)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return nil, err
	}

	return body, nil
}

func ExtractLocalTarball(source string, config config.IConfig) error {
	color := ansi.Color(os.Stdout)
	fmt.Println(color.Yellow(fmt.Sprintf("extracting tarball at %s...", source)))

	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()

	gzf, err := gzip.NewReader(f)
	if err != nil {
		return err
	}

	tarReader := tar.NewReader(gzf)
	err = extractFromTarball(tarReader, config)
	if err != nil {
		return err
	}

	return nil
}

// FetchAndExtractRemoteTarball returns the remote tarball body
func FetchAndExtractRemoteTarball(url string, config config.IConfig) error {
	color := ansi.Color(os.Stdout)
	fmt.Println(color.Yellow(fmt.Sprintf("fetching tarball at %s...", url)))

	t := &requests.TracedTransport{}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	trace := &httptrace.ClientTrace{
		GotConn: t.GotConn,
		DNSDone: t.DNSDone,
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	client := &http.Client{Transport: t}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	archive, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}

	defer archive.Close()

	tarReader := tar.NewReader(archive)
	err = extractFromTarball(tarReader, config)
	if err != nil {
		return err
	}

	return nil
}

func extractFromTarball(tarReader *tar.Reader, config config.IConfig) error {
	var manifest PluginList
	var pluginData []byte
	color := ansi.Color(os.Stdout)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		name := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if name == "manifest.toml" {
				tomlBytes, _ := ioutil.ReadAll(tarReader)
				err = toml.Unmarshal(tomlBytes, &manifest)
				if err != nil {
					return err
				}

				fmt.Println(color.Green(fmt.Sprintf("✔ extracted manifest '%s'", name)))
			} else if strings.Contains(name, "stripe-cli-") {
				pluginData, _ = ioutil.ReadAll(tarReader)
				fmt.Println(color.Green(fmt.Sprintf("✔ extracted plugin '%s'", name)))
			}

		default:
			return fmt.Errorf("unrecognized file type for file %s: %c", name, header.Typeflag)
		}
	}

	// update plugin manifest and config manifest
	if len(manifest.Plugins) == 1 && len(pluginData) > 0 {
		plugin := manifest.Plugins[0]
		err := AddEntryToPluginManifest(plugin, config)
		if err != nil {
			return err
		}

		fs := afero.NewOsFs()
		err = plugin.verifychecksumAndSavePlugin(pluginData, config, fs, plugin.Releases[0].Version)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("missing required manifest.toml or plugin in the archive")
	}

	return nil
}

// CleanupAllClients tears down and disconnects all "managed" plugin clients
func CleanupAllClients() {
	log.Debug("Tearing down plugin before exit")
	hcplugin.CleanupClients()
}

// IsPluginCommand returns true if the command invoked is for a plugin
// false otherwise
func IsPluginCommand(cmd *cobra.Command) bool {
	isPlugin := false

	for key, value := range cmd.Annotations {
		if key == "scope" && value == "plugin" {
			isPlugin = true
		}
	}

	return isPlugin
}
