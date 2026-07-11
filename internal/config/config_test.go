package config

import "testing"

func TestValidateRejectsDevelopmentDriverByDefault(t *testing.T) {
	cfg := Config{
		DriverName:     DevelopmentDriverName,
		NodeName:       "worker-1",
		ProbeTimeout:   1,
		HealthInterval: 1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected development driver validation error")
	}
}

func TestValidateAcceptsExplicitDevelopmentDriver(t *testing.T) {
	cfg := Config{
		DriverName:       DevelopmentDriverName,
		NodeName:         "worker-1",
		ProbeTimeout:     1,
		HealthInterval:   1,
		AllowDevelopment: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
