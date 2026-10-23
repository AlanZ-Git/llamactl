package config

// N1 is locally administered. Paths and engine identity cannot be supplied by
// remote evaluation clients. Zero limits select finite engineering defaults.
type N1Config struct {
	Instance                       string            `yaml:"instance"`
	Roots                          map[string]string `yaml:"roots"`
	EngineIdentity                 string            `yaml:"engine_identity"`
	AdministratorReadOnlyPlacement bool              `yaml:"administrator_read_only_placement"`
	MaxWallSeconds                 int               `yaml:"max_wall_seconds"`
	MaxRequests                    int64             `yaml:"max_requests"`
	MaxTokens                      int64             `yaml:"max_tokens"`
	MaxBytes                       int64             `yaml:"max_bytes"`
	MaxBodyBytes                   int64             `yaml:"max_body_bytes"`
	DeliveryTTLSeconds             int               `yaml:"delivery_ttl_seconds"`
	ServiceAbsoluteSeconds         int               `yaml:"service_absolute_seconds"`
}
