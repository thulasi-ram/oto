// Package worker holds the notification queue workers: notify.evaluate, deliver.dispatch, the notify.digest tick and the notify.digest.reconcile detector, which reports gaps and delivers nothing. ⛔ There is no unacked-reminder sweep: the owner withdrew it (git-bug bd0fb1d) and oto sends nothing unprompted.
package worker
