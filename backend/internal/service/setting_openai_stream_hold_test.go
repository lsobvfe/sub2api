//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAIStreamHoldSettingRepo struct {
	*mockSettingRepo
	getErr error
	setErr error
}

func newOpenAIStreamHoldSettingRepo() *openAIStreamHoldSettingRepo {
	return &openAIStreamHoldSettingRepo{mockSettingRepo: newMockSettingRepo()}
}

func (r *openAIStreamHoldSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	value, err := r.mockSettingRepo.GetValue(ctx, key)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *openAIStreamHoldSettingRepo) Set(ctx context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	return r.mockSettingRepo.Set(ctx, key, value)
}

func TestLoadOpenAIStreamHoldRuntimeInitializesFromConfig(t *testing.T) {
	repo := newOpenAIStreamHoldSettingRepo()
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			OpenAIStreamHold: config.GatewayOpenAIStreamHoldConfig{Enabled: true},
		},
	}
	svc := NewSettingService(repo, cfg)

	require.NoError(t, svc.LoadOpenAIStreamHoldRuntime(context.Background()))
	require.True(t, svc.IsOpenAIStreamHoldEnabled())
	require.Equal(t, "true", repo.data[SettingKeyOpenAIStreamHoldEnabled])
}

func TestLoadOpenAIStreamHoldRuntimeUsesPersistedValue(t *testing.T) {
	repo := newOpenAIStreamHoldSettingRepo()
	repo.data[SettingKeyOpenAIStreamHoldEnabled] = "false"
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			OpenAIStreamHold: config.GatewayOpenAIStreamHoldConfig{Enabled: true},
		},
	}
	svc := NewSettingService(repo, cfg)

	require.NoError(t, svc.LoadOpenAIStreamHoldRuntime(context.Background()))
	require.False(t, svc.IsOpenAIStreamHoldEnabled())
}

func TestLoadOpenAIStreamHoldRuntimeRejectsInvalidPersistedValue(t *testing.T) {
	repo := newOpenAIStreamHoldSettingRepo()
	repo.data[SettingKeyOpenAIStreamHoldEnabled] = "enabled"
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			OpenAIStreamHold: config.GatewayOpenAIStreamHoldConfig{Enabled: true},
		},
	}
	svc := NewSettingService(repo, cfg)

	require.ErrorContains(t, svc.LoadOpenAIStreamHoldRuntime(context.Background()), "must be true or false")
	require.False(t, svc.IsOpenAIStreamHoldEnabled())
}

func TestSetOpenAIStreamHoldSettingsUpdatesRuntimeAfterPersistence(t *testing.T) {
	repo := newOpenAIStreamHoldSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	require.NoError(t, svc.SetOpenAIStreamHoldSettings(
		context.Background(),
		OpenAIStreamHoldSettings{Enabled: true},
	))
	require.True(t, svc.IsOpenAIStreamHoldEnabled())
	require.Equal(t, "true", repo.data[SettingKeyOpenAIStreamHoldEnabled])

	repo.setErr = errors.New("write failed")
	require.ErrorContains(t, svc.SetOpenAIStreamHoldSettings(
		context.Background(),
		OpenAIStreamHoldSettings{Enabled: false},
	), "write failed")
	require.True(t, svc.IsOpenAIStreamHoldEnabled())
}
