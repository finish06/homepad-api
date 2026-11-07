package api

// SPEC-backup-import §8 — export, restore and Homepage import.

// Version is the Homepad version written into a backup's homepadVersion. It
// is set at build time (-ldflags "-X …/internal/api.Version=17.1.0"); nothing
// reads it back (§2.3).
var Version = "dev"
