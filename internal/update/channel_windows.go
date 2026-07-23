//go:build windows

package update

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	machineUpdatePolicyKey   = `SOFTWARE\DouyinLive\Updater`
	machineUpdateChannelName = "Channel"
)

func ProductionUpdateChannel() (string, error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		machineUpdatePolicyKey,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if errors.Is(err, registry.ErrNotExist) {
		return ProductionChannel, nil
	}
	if err != nil {
		return "", errors.New("UPDATE_CHANNEL_POLICY_UNAVAILABLE")
	}
	defer key.Close()
	value, _, err := key.GetStringValue(machineUpdateChannelName)
	if errors.Is(err, registry.ErrNotExist) {
		return ProductionChannel, nil
	}
	if err != nil {
		return "", errors.New("UPDATE_CHANNEL_POLICY_UNAVAILABLE")
	}
	return validateProductionUpdateChannel(value, true)
}

func validateProductionUpdateChannel(value string, configured bool) (string, error) {
	if !configured {
		return ProductionChannel, nil
	}
	value = strings.TrimSpace(strings.ToLower(value))
	if value != "stable" && value != "canary" {
		return "", errors.New("UPDATE_CHANNEL_POLICY_INVALID")
	}
	return value, nil
}
