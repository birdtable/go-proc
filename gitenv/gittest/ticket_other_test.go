//go:build !windows

package gittest

// fileTicketCache is true off Windows, where libcurl's Negotiate takes the GSS-API's ticket cache,
// which KRB5CCNAME names, so a cache a test plants there is what git sends.
const fileTicketCache = true
