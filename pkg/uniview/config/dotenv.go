package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv loads environment variables from .env and optionally ENV_FILE.
// If ENV_FILE is set, it is loaded after .env and overrides its values.
// Variables already defined in the process environment are never overridden.
func LoadDotEnv() error {
	existingEnv := existingEnvKeys()

	if err := loadEnvFile(".env", existingEnv, false); err != nil {
		return err
	}

	if envFile := strings.TrimSpace(os.Getenv("ENV_FILE")); envFile != "" {
		if err := loadEnvFile(envFile, existingEnv, true); err != nil {
			return err
		}
	}

	return nil
}

func existingEnvKeys() map[string]struct{} {
	keys := make(map[string]struct{})
	for _, env := range os.Environ() {
		key, _, ok := strings.Cut(env, "=")
		if ok && key != "" {
			keys[key] = struct{}{}
		}
	}
	return keys
}

func loadEnvFile(path string, existingEnv map[string]struct{}, allowOverride bool) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export"))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := existingEnv[key]; exists {
			continue
		}
		if !allowOverride {
			if _, exists := os.LookupEnv(key); exists {
				continue
			}
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
