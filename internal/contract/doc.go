// Package contract holds api/contract.txt, the promises a product may rely on, each naming the test
// that holds it, to the format its tests hold it to. It has no code of its own; its tests read the
// file, the module's test files, and the last tag through git.
package contract
