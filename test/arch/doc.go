// Package arch holds the gates on CONTEXT.md §4: the module-direction gate, which
// reads the real import graph and fails on any cross-module edge §4 does not draw,
// and the table-ownership gate, which reads SQL string literals and fails on any
// table `internal/drill` names that §4's mechanism 4 does not declare.
package arch
