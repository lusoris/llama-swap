package config

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func LoadConfigFromReader(r io.Reader) (Config, error) {
	config, err := decodeConfigWithMacros(r)
	if err != nil {
		return Config{}, err
	}
	if err := applyGlobalDefaultsAndValidate(&config); err != nil {
		return Config{}, err
	}
	if err := normalizeAndValidateModels(&config); err != nil {
		return Config{}, err
	}
	if err := resolveRouting(&config); err != nil {
		return Config{}, err
	}
	if err := validateGroupsAndMatrix(&config); err != nil {
		return Config{}, err
	}
	if err := finalizeRouting(&config); err != nil {
		return Config{}, err
	}
	if err := finalizeConfig(&config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func decodeConfigWithMacros(r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, err
	}
	yamlStr := string(data)

	// Phase 1: Substitute all ${env.VAR} macros at string level
	// This is safe because env values are simple strings without YAML formatting
	yamlStr, err = substituteEnvMacros(yamlStr)
	if err != nil {
		return Config{}, err
	}

	raw, macroConfig, err := resolveConfigMacros(yamlStr)
	if err != nil {
		return Config{}, err
	}

	var node yaml.Node
	if err = node.Encode(raw); err != nil {
		return Config{}, err
	}

	// Decode the resolved values into the full Config with defaults.
	config := Config{
		HealthCheckTimeout: 120,
		StartPort:          5800,
		LogLevel:           "info",
		LogTimeFormat:      "",
		LogToStdout:        LogToStdoutProxy,
		MetricsMaxInMemory: 1000,
		CaptureBuffer:      5,
		GlobalTTL:          0,
		UnloadTimeout:      DEFAULT_UNLOAD_TIMEOUT,
		UI: UIConfig{Activity: UIActivityConfig{SessionID: []string{
			"X-Session-ID",
			"X-Litellm-Session-Id",
		}}},
	}
	if err = node.Decode(&config); err != nil {
		return Config{}, err
	}
	config.Macros = macroConfig.Macros
	for modelID, modelConfig := range config.Models {
		modelConfig.Macros = macroConfig.Models[modelID].Macros
		config.Models[modelID] = modelConfig
	}
	return config, nil
}

func applyGlobalDefaults(config *Config) {
	if config.HealthCheckTimeout < 15 {
		config.HealthCheckTimeout = 15
	}
	if config.Performance.Every == 0 {
		config.Performance.Every = 5 * time.Second
	}
	if config.UnloadTimeout == 0 {
		config.UnloadTimeout = DEFAULT_UNLOAD_TIMEOUT
	}
	config.UI.Activity.SessionID = normalizeHeaderNames(config.UI.Activity.SessionID)
	if len(config.Upstream.IgnorePaths) == 0 {
		config.Upstream.IgnorePaths = DefaultUpstreamIgnorePaths()
	}
}

func validateGlobalLimits(config *Config) error {
	if config.StartPort < 1 {
		return fmt.Errorf("startPort must be greater than 1")
	}
	if config.GlobalTTL < 0 {
		return fmt.Errorf("globalTTL must be >= 0")
	}
	if config.UnloadTimeout < 0 {
		return fmt.Errorf("unloadTimeout must be >= 0")
	}
	if config.GlobalConcurrencyLimit < 0 {
		return fmt.Errorf("globalConcurrencyLimit must be >= 0")
	}
	return nil
}

func validateGlobalSettings(config *Config) error {
	if err := config.Performance.Validate(); err != nil {
		return fmt.Errorf("performance: %w", err)
	}
	if err := validateGlobalLimits(config); err != nil {
		return err
	}
	if config.Store != nil {
		if err := validateStorePath(config.Store.Path); err != nil {
			return err
		}
	}
	if err := config.Security.CORS.Validate(); err != nil {
		return fmt.Errorf("security.cors: %w", err)
	}
	if _, _, _, err := ParseLogToStdout(config.LogToStdout); err != nil {
		return err
	}
	return nil
}

func applyGlobalDefaultsAndValidate(config *Config) error {
	applyGlobalDefaults(config)
	return validateGlobalSettings(config)
}

func populateModelAliases(config *Config) error {
	config.aliases = make(map[string]string)
	for modelName, modelConfig := range config.Models {
		for _, alias := range modelConfig.Aliases {
			if _, found := config.aliases[alias]; found {
				return fmt.Errorf("duplicate alias %s found in model: %s", alias, modelName)
			}
			config.aliases[alias] = modelName
		}
	}
	return nil
}

func normalizeAndValidateModels(config *Config) error {
	if err := populateModelAliases(config); err != nil {
		return err
	}

	// Sort model IDs for deterministic validation and normalization.
	modelIds := make([]string, 0, len(config.Models))
	for modelId := range config.Models {
		modelIds = append(modelIds, modelId)
	}
	sort.Strings(modelIds)

	for _, modelId := range modelIds {
		if err := normalizeModel(config, modelId); err != nil {
			return err
		}
	}
	return nil
}

func applyComfyUIDefaults(modelConfig *ModelConfig, modelId string) {
	if modelId != ComfyUIModelID {
		return
	}
	if modelConfig.ConcurrencyLimit < comfyUIConcurrencyLimit {
		modelConfig.ConcurrencyLimit = comfyUIConcurrencyLimit
	}
	modelConfig.Compat.IgnoreWebsockets = true
}

func normalizeModelTimeouts(modelConfig *ModelConfig, config *Config, modelId string) error {
	// set model TTL to globalTTL it is the default value
	if modelConfig.UnloadAfter == MODEL_CONFIG_DEFAULT_TTL {
		modelConfig.UnloadAfter = config.GlobalTTL
	}
	if modelConfig.UnloadAfter < 0 {
		return fmt.Errorf("model %s: invalid TTL value %d", modelId, modelConfig.UnloadAfter)
	}

	// set model unloadTimeout to the global value when left at the default
	if modelConfig.UnloadTimeout < 0 {
		return fmt.Errorf("model %s: invalid unloadTimeout value %d", modelId, modelConfig.UnloadTimeout)
	}
	if modelConfig.UnloadTimeout == 0 {
		modelConfig.UnloadTimeout = config.UnloadTimeout
	}
	return nil
}

func normalizeModel(config *Config, modelId string) error {
	modelConfig := config.Models[modelId]
	modelConfig.HealthCheckTimeout = config.HealthCheckTimeout
	applyComfyUIDefaults(&modelConfig, modelId)

	if err := normalizeModelTimeouts(&modelConfig, config, modelId); err != nil {
		return err
	}

	if err := modelConfig.Capabilities.Validate(); err != nil {
		return fmt.Errorf("model %s: %w", modelId, err)
	}

	if err := registerFilterAliases(config, modelId, &modelConfig); err != nil {
		return err
	}

	if _, err := url.Parse(modelConfig.Proxy); err != nil {
		return fmt.Errorf("model %s: invalid proxy URL: %w", modelId, err)
	}

	if modelConfig.SendLoadingState == nil {
		v := config.SendLoadingState
		modelConfig.SendLoadingState = &v
	}

	config.Models[modelId] = modelConfig
	return nil
}

func registerFilterAliases(config *Config, modelId string, modelConfig *ModelConfig) error {
	// Auto-register setParamsByID keys as aliases (skip the model's own ID)
	for key := range modelConfig.Filters.SetParamsByID {
		if key == modelId {
			continue
		}
		if _, exists := config.Models[key]; exists {
			return fmt.Errorf("model %s filters.setParamsByID: key '%s' conflicts with an existing model ID", modelId, key)
		}
		if existingModel, exists := config.aliases[key]; exists {
			if existingModel != modelId {
				return fmt.Errorf("duplicate alias '%s' in model %s filters.setParamsByID, already used by model %s", key, modelId, existingModel)
			}
			continue // already registered as explicit alias for this model
		}
		config.aliases[key] = modelId
		modelConfig.Aliases = append(modelConfig.Aliases, key)
	}
	return nil
}

func hasLegacyRouting(config *Config) bool {
	return config.Matrix != nil || len(config.Groups) > 0
}

func hasBlockRouting(rtr RouterConfig) bool {
	return rtr.Use != "" || rtr.Settings.Matrix != nil || len(rtr.Settings.Groups) > 0
}

func resolveRouterSettings(config *Config) error {
	// Both groups and matrix may be defined under routing.router.settings;
	// routing.router.use selects which one is active, so there is no conflict.
	rs := config.Routing.Router.Settings
	switch config.Routing.Router.Use {
	case "matrix":
		if rs.Matrix == nil {
			return fmt.Errorf("routing.router.use is 'matrix' but routing.router.settings.matrix is not set")
		}
		config.Matrix = rs.Matrix
	case "group", "":
		config.Groups = rs.Groups
	default:
		return fmt.Errorf("routing.router.use: unknown router %q (valid: group, matrix)", config.Routing.Router.Use)
	}
	return nil
}

func resolveRouting(config *Config) error {
	// Normalize routing config. The legacy top-level `matrix`/`groups` keys and
	// the new `routing.router` block are mutually exclusive: a config may use
	// either style, never both.
	hasTopLevel := hasLegacyRouting(config)
	hasRouting := hasBlockRouting(config.Routing.Router)

	if hasTopLevel && hasRouting {
		return fmt.Errorf("config uses both the legacy top-level 'matrix'/'groups' keys and the new 'routing.router' block; please migrate the top-level keys into 'routing.router' and remove them")
	}

	if !hasTopLevel {
		if err := resolveRouterSettings(config); err != nil {
			return err
		}
	}

	// groups XOR matrix
	if config.Matrix != nil && len(config.Groups) > 0 {
		return fmt.Errorf("config cannot use both 'groups' and 'matrix'")
	}
	return nil
}

func validateGroupsAndMatrix(config *Config) error {
	if config.Matrix != nil {
		if err := ValidateMatrix(config.Matrix, config.Models); err != nil {
			return fmt.Errorf("matrix: %w", err)
		}
		return nil
	}

	*config = AddDefaultGroupToConfig(*config)

	// Validate group members
	memberUsage := make(map[string]string)
	for groupID, groupConfig := range config.Groups {
		prevSet := make(map[string]bool)
		for _, member := range groupConfig.Members {
			if _, found := prevSet[member]; found {
				return fmt.Errorf("duplicate model member %s found in group: %s", member, groupID)
			}
			prevSet[member] = true

			if existingGroup, exists := memberUsage[member]; exists {
				return fmt.Errorf("model member %s is used in multiple groups: %s and %s", member, existingGroup, groupID)
			}
			memberUsage[member] = groupID
		}
	}
	return nil
}

func finalizeRouting(config *Config) error {
	// Build the canonical Config.Routing from the effective result. Both legacy
	// and new-style configs converge here. The Matrix pointer is shared so the
	// compiled matrix program stays in one place.
	if config.Matrix != nil {
		config.Routing.Router.Use = "matrix"
	} else {
		config.Routing.Router.Use = "group"
	}
	config.Routing.Router.Settings.Matrix = config.Matrix
	config.Routing.Router.Settings.Groups = config.Groups

	if config.Routing.Scheduler.Use == "" {
		config.Routing.Scheduler.Use = "fifo"
	}
	if config.Routing.Scheduler.Use != "fifo" {
		return fmt.Errorf("routing.scheduler.use: unknown scheduler %q (valid: fifo)", config.Routing.Scheduler.Use)
	}
	for modelID := range config.Routing.Scheduler.Settings.Fifo.Priority {
		if _, found := config.RealModelName(modelID); !found {
			return fmt.Errorf("routing.scheduler.settings.fifo.priority references unknown model %q", modelID)
		}
	}
	return nil
}

func cleanupStartupPreload(config *Config) {
	if len(config.Hooks.OnStartup.Preload) == 0 {
		return
	}
	var toPreload []string
	for _, modelID := range config.Hooks.OnStartup.Preload {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		if real, found := config.RealModelName(modelID); found {
			toPreload = append(toPreload, real)
		}
	}
	config.Hooks.OnStartup.Preload = toPreload
}

func validateAPIKeys(config *Config) error {
	for i, apikey := range config.RequiredAPIKeys {
		if apikey == "" {
			return fmt.Errorf("empty api key found in apiKeys")
		}
		if strings.Contains(apikey, " ") {
			return fmt.Errorf("apiKeys[%d]: api key cannot contain spaces", i)
		}
		config.RequiredAPIKeys[i] = apikey
	}
	return nil
}

func finalizeConfig(config *Config) error {
	cleanupStartupPreload(config)

	if err := validateAPIKeys(config); err != nil {
		return err
	}

	if err := ValidatePeerNamespace(*config); err != nil {
		return err
	}

	if err := validateSelectors(*config); err != nil {
		return err
	}

	if err := validateProfiles(*config); err != nil {
		return err
	}

	if err := validateTailcatConfig(config); err != nil {
		return err
	}
	return nil
}

func validateProfiles(config Config) error {
	for profileName, profile := range config.Profiles {
		if strings.TrimSpace(profileName) == "" {
			return fmt.Errorf("profiles: profile names cannot be empty")
		}
		if len(profile.Pins) == 0 {
			return fmt.Errorf("profiles.%s.pins must contain at least one entry", profileName)
		}
		for pin, target := range profile.Pins {
			if strings.TrimSpace(pin) == "" {
				return fmt.Errorf("profiles.%s.pins: pin names cannot be empty", profileName)
			}
			if target == "" {
				continue
			}
			if _, found := config.ResolveBaseModel(target); !found {
				if _, found := config.Selectors[target]; found {
					continue
				}
				return fmt.Errorf("profiles.%s.pins.%s references unknown model %q", profileName, pin, target)
			}
		}
	}
	if profile := config.Hooks.OnStartup.Profile; profile != "" {
		if _, found := config.Profiles[profile]; !found {
			return fmt.Errorf("hooks.on_startup.profile references unknown profile %q", profile)
		}
	}
	return nil
}

func normalizeHeaderNames(names []string) []string {
	normalized := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized
}
