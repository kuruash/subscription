// Package cache owns the Redis key format so every caller (API service,
// background worker, future admin tools) uses the same string. If this
// helper were duplicated across packages, a typo in one copy would
// silently split reads from invalidations.
package cache

import "strconv"

// UserListKey is the key for a user's cached subscription list.
func UserListKey(userID int) string {
	return "user:" + strconv.Itoa(userID) + ":subscriptions"
}
