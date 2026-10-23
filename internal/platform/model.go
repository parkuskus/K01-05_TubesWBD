package platform

import "if3110.local/db-platform/internal/contracts"

// User is the public platform account summary; password hashes are never exposed.
type User struct {
	ID              contracts.UserID
	Username        string
	IsPlatformAdmin bool
}

type Group struct {
	ID   contracts.GroupID
	Name string
}

type Grant struct {
	GroupID    contracts.GroupID
	DatabaseID contracts.DatabaseID
	Level      contracts.AccessLevel
}
