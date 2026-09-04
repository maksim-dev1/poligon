// Package model holds the core domain types shared across poligon.
package model

import "time"

// Platform is the device OS family.
type Platform string

const (
	Android Platform = "android"
	IOS     Platform = "ios"
)

// DeviceStatus is the lifecycle state of a device in the pool.
type DeviceStatus string

const (
	StatusOffline      DeviceStatus = "offline"      // not physically connected
	StatusUnauthorized DeviceStatus = "unauthorized" // connected but adb/pairing not approved on the device
	StatusFree         DeviceStatus = "free"         // connected, nobody holds it
	StatusReserved     DeviceStatus = "reserved"     // held by a user, no live session yet
	StatusBusy         DeviceStatus = "busy"         // live session open / install running
	StatusRunningTest  DeviceStatus = "running"      // automated job in progress
	StatusDegraded     DeviceStatus = "degraded"     // flapping online/offline, excluded from scheduling
	StatusMaintenance  DeviceStatus = "maintenance"  // manually pulled out of the pool
)

// Source records how a device entered the pool.
type Source string

const (
	SourceConfig Source = "config" // declared in devices.yaml
	SourceAuto   Source = "auto"   // discovered on connect
)

// Device is one phone wired to the farm host.
type Device struct {
	ID       string       `json:"id"` // stable human id, e.g. "pixel6-01"
	Platform Platform     `json:"platform"`
	Serial   string       `json:"serial"` // adb serial (android)
	UDID     string       `json:"udid"`   // device udid (ios)
	Tags     []string     `json:"tags"`
	Status   DeviceStatus `json:"status"`
	Source   Source       `json:"source"`
	Specs    Specs        `json:"specs"`
	LastSeen time.Time    `json:"last_seen"`
	// Adopted is true once the device has been prepared and admitted to the
	// pool. Config devices are adopted on load; auto-discovered devices start
	// as candidates (adopted=false) until a user runs "connect to farm".
	Adopted bool `json:"adopted"`
}

// Specs are hardware/OS characteristics, refreshed periodically.
type Specs struct {
	Model         string `json:"model"`
	Manufacturer  string `json:"manufacturer"`
	SoC           string `json:"soc"`
	OSVersion     string `json:"os_version"`
	APILevel      string `json:"api_level,omitempty"` // android
	Build         string `json:"build,omitempty"`     // ios
	RAM           string `json:"ram"`
	Storage       string `json:"storage"`
	ScreenSize    string `json:"screen_size"` // px, e.g. "1080x2400"
	ScreenDensity string `json:"screen_density"`
	Battery       int    `json:"battery"` // percent, -1 unknown
	BatteryTempC  string `json:"battery_temp_c,omitempty"`
}

// Reservation is a user's hold on a device.
type Reservation struct {
	ID        int64     `json:"id"`
	DeviceID  string    `json:"device_id"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"` // hard cap
	RenewedAt time.Time `json:"renewed_at"` // last heartbeat
	Released  bool      `json:"released"`
}

// User is a farm account.
type User struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"-"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}
