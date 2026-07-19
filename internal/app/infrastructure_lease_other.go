//go:build !windows

package app

type noopApplicationInstanceLease struct{}

func acquireApplicationInstanceLease(string) (applicationInstanceLease, error) {
	return &noopApplicationInstanceLease{}, nil
}

func (*noopApplicationInstanceLease) Close() error {
	return nil
}
