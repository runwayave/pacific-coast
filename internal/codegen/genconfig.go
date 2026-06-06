package codegen

// defaultModulePrefix is the import path of the central atlantis-go SDK
// module. Used when GenConfig.ModulePrefix is empty so the in-repo
// `tide generate` keeps emitting the same paths it always has.
const defaultModulePrefix = "github.com/rachitkumar205/atlantis-go"

// GenConfig parameterizes the Go import paths the client emitters write.
// The zero value reproduces the central SDK layout; caller-local
// generation sets ModulePrefix to "<caller-module>/<output_dir>" so the
// generated wrappers import the caller's own pb packages.
type GenConfig struct {
	ModulePrefix string
}

// pbImportPrefix is the path the generated client wrappers import their
// proto types from, minus the "/pb/atlantis/<ns>/v1" suffix.
func (c GenConfig) pbImportPrefix() string {
	if c.ModulePrefix == "" {
		return defaultModulePrefix
	}
	return c.ModulePrefix
}
