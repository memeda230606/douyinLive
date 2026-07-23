package update

import (
	"crypto/ed25519"
	"fmt"
)

const (
	ProductionBaseURL = "https://douyinlive-updates-cn-hangzhou-1e8d9993065b.oss-cn-hangzhou.aliyuncs.com"
	ProductionChannel = "stable"
	ProductionKeyID   = "release-2026-01"
	productionKeyHex  = "5ab90aa786227be9a69fd9649719de25717c3492521a1b38c5e24793b40ef00e"
)

func ProductionTrustedKeys() (map[string]ed25519.PublicKey, error) {
	publicKey, err := DecodePublicKey(productionKeyHex)
	if err != nil {
		return nil, fmt.Errorf("UPDATE_TRUST_CONFIGURATION_INVALID: %w", err)
	}
	return map[string]ed25519.PublicKey{ProductionKeyID: publicKey}, nil
}
