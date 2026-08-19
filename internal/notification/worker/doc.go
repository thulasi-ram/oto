// Package worker holds the notification queue workers: notify.evaluate, deliver.dispatch and the notify.digest tick. ⛔ There is no unacked-reminder sweep: the owner withdrew it (git-bug bd0fb1d) and oto sends nothing unprompted.
package worker
