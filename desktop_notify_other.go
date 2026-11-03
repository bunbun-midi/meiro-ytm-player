//go:build !windows || (!amd64 && !arm64)

package main

func notificationArtworkNeeded() bool { return false }

func prepareNotificationArtwork(*desktop, []byte) error { return nil }

func applyNotificationArtwork(*desktop, string, string, []byte) error { return nil }

func destroyNotificationIcon(uintptr) {}

func showTrackToast(*desktop, string, string, []byte) bool { return false }

func dismissTrackToast() {}

func registerToastAppID() {}
