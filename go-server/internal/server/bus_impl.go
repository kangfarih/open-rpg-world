// Bus adapter over the D2a package seams (transport + registry).
//
// rootBus implements net.Bus (internal/net/bus.go) by delegating to the
// canonical owners: Broadcast/Unicast -> internal/world Registry (region
// routing over the entity + connection tables), RegionOf/Interested ->
// internal/world region math. It introduces no behavior change; the world
// Registry owns the maps and the net Hub owns the sockets.
package server

import (
	opsnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
)

// rootBus adapts the world Registry to the net.Bus seam.
type rootBus struct{}

// Compile-time assertion that rootBus satisfies the seam.
var _ opsnet.Bus = rootBus{}

// Broadcast routes each frame by region scope (world: Broadcast).
func (rootBus) Broadcast(frames ...opsnet.Frame) { worldcore.Broadcast(frames...) }

// SendTo unicasts frames to one player instance (world: Unicast via the net
// send path); unknown instances are dropped like the old enqueueTo
// unknown-conn no-op.
func (rootBus) SendTo(instance string, frames ...opsnet.Frame) {
	worldcore.Unicast(instance, frames...)
}

// RegionOf maps a tile to its region id (world: TileRegion).
func (rootBus) RegionOf(x, y int) int { return worldcore.TileRegion(x, y) }

// Interested reports whether an entity region is visible to a client
// centered in clientRegion (world: InterestedRegion over the surrounding
// 9-region interest set).
func (rootBus) Interested(region, clientRegion int) bool {
	return worldcore.InterestedRegion(region, clientRegion)
}
