//go:build !windows

package main

import (
	_ "github.com/pion/mediadevices/pkg/driver/audiotest"
	_ "github.com/pion/mediadevices/pkg/driver/screen"
	_ "github.com/pion/mediadevices/pkg/driver/videotest"
)
