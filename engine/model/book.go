// Package model defines provider-neutral audiobook contracts.
package model

import (
	"errors"
	"regexp"
)

var (
	ErrInvalidBookID  = errors.New("invalid book identity")
	namespacePattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	asinPattern       = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)
)

// BookID is a comparable identity, independent of title, local path and playback.
// Namespace distinguishes provider catalogs, such as Audible marketplaces.
// Fields are not normalized: callers must not silently change stored identities.
type BookID struct {
	Provider  string `json:"provider"`
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

// Validate checks identity syntax, not provider support or account entitlement.
// Audible requires an ASIN; support for a marketplace is checked by its adapter.
// Errors intentionally omit input that could contain private provider data.
func (id BookID) Validate() error {
	if !namespacePattern.MatchString(id.Provider) || !namespacePattern.MatchString(id.Namespace) {
		return ErrInvalidBookID
	}
	if !identifierPattern.MatchString(id.ID) {
		return ErrInvalidBookID
	}
	if id.Provider == "audible" && !asinPattern.MatchString(id.ID) {
		return ErrInvalidBookID
	}
	return nil
}
