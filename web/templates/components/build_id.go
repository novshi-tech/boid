package components

import (
	"fmt"
	"time"
)

// BuildID is appended as a ?v= query parameter to static asset URLs that
// evolve at runtime-rebuild granularity (boid-terminal-init.js and anything
// it imports). Regenerated on every daemon startup so a restart reliably
// busts browser HTTP caches — ES modules in particular are cached hard
// across reloads and do not respect regular cache-control hints.
var BuildID = fmt.Sprintf("%d", time.Now().UnixNano())
