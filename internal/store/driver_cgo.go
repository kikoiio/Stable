//go:build linux || darwin || windows

package store

import _ "github.com/mattn/go-sqlite3"

const driverAvailable = true
