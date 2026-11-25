package gittest

// fileTicketCache is false on Windows, where libcurl's Negotiate takes the logon session through
// SSPI and no file cache, so a cache a test plants is never sent.
const fileTicketCache = false
