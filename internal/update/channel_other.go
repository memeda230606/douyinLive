//go:build !windows

package update

func ProductionUpdateChannel() (string, error) {
	return ProductionChannel, nil
}
