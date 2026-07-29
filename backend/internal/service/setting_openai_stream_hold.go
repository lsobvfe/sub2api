package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

type OpenAIStreamHoldSettings struct {
	Enabled bool `json:"enabled"`
}

// LoadOpenAIStreamHoldRuntime materializes the startup configuration into the
// settings store once, then makes the persisted value authoritative.
func (s *SettingService) LoadOpenAIStreamHoldRuntime(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("openai stream hold setting repository is not configured")
	}

	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAIStreamHoldEnabled)
	if errors.Is(err, ErrSettingNotFound) {
		enabled := s.cfg != nil && s.cfg.Gateway.OpenAIStreamHold.Enabled
		value = strconv.FormatBool(enabled)
		if setErr := s.settingRepo.Set(ctx, SettingKeyOpenAIStreamHoldEnabled, value); setErr != nil {
			s.openAIStreamHoldEnabled.Store(false)
			return fmt.Errorf("initialize %s: %w", SettingKeyOpenAIStreamHoldEnabled, setErr)
		}
		s.openAIStreamHoldEnabled.Store(enabled)
		slog.Info("openai.stream_hold_runtime_initialized", "enabled", enabled)
		return nil
	}
	if err != nil {
		s.openAIStreamHoldEnabled.Store(false)
		return fmt.Errorf("load %s: %w", SettingKeyOpenAIStreamHoldEnabled, err)
	}

	enabled, err := parseOpenAIStreamHoldEnabled(value)
	if err != nil {
		s.openAIStreamHoldEnabled.Store(false)
		return err
	}
	s.openAIStreamHoldEnabled.Store(enabled)
	slog.Info("openai.stream_hold_runtime_loaded", "enabled", enabled)
	return nil
}

func parseOpenAIStreamHoldEnabled(value string) (bool, error) {
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", SettingKeyOpenAIStreamHoldEnabled)
	}
}

func (s *SettingService) GetOpenAIStreamHoldSettings() OpenAIStreamHoldSettings {
	return OpenAIStreamHoldSettings{Enabled: s.IsOpenAIStreamHoldEnabled()}
}

func (s *SettingService) IsOpenAIStreamHoldEnabled() bool {
	return s != nil && s.openAIStreamHoldEnabled.Load()
}

func (s *SettingService) SetOpenAIStreamHoldSettings(ctx context.Context, settings OpenAIStreamHoldSettings) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("openai stream hold setting repository is not configured")
	}
	value := strconv.FormatBool(settings.Enabled)
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAIStreamHoldEnabled, value); err != nil {
		logger.FromContext(ctx).Warn("openai.stream_hold_runtime_update_failed",
			zap.Bool("enabled", settings.Enabled),
			zap.Error(err),
		)
		return fmt.Errorf("set %s: %w", SettingKeyOpenAIStreamHoldEnabled, err)
	}
	s.openAIStreamHoldEnabled.Store(settings.Enabled)
	logger.FromContext(ctx).Info("openai.stream_hold_runtime_updated",
		zap.Bool("enabled", settings.Enabled),
	)
	return nil
}
