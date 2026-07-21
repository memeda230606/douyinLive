//go:build p3accacceptance

package main

import "errors"

var (
	errP3ACCRelayAddress       = errors.New("P3ACC_RELAY_ADDRESS_INVALID")
	errP3ACCRelayAnnouncement  = errors.New("P3ACC_RELAY_ANNOUNCEMENT_FAILED")
	errP3ACCRelayConfiguration = errors.New("P3ACC_RELAY_CONFIGURATION_INVALID")
	errP3ACCRelayAccept        = errors.New("P3ACC_RELAY_ACCEPT_FAILED")
)
