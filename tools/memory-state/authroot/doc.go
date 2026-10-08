// Package authroot is an isolated D-03 experiment, not a production projection
// format or a runtime trust anchor. It exercises bounded authenticated pages, disk COW updates, root publication
// and complete ordered traversal with standard SHA-256/HMAC-SHA-256 primitives.
// It deliberately has no metadata baseline, key lifecycle, HA, production recovery, cache, or garbage-collection integration.
package authroot
