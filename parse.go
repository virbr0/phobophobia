package phobos

// parse.go - server -> client message parsing, a port of the relevant parts
// of client/cl_parse.c, client/cl_ents.c and client/cl_tent.c.

import (
	"fmt"
)

// server -> client ops (qcommon/qcommon.h, enum svc_ops_e)
const (
	svcBad = iota
	svcMuzzleFlash
	svcMuzzleFlash2
	svcTempEntity
	svcLayout
	svcInventory
	svcNop
	svcDisconnect
	svcReconnect
	svcSound
	svcPrint
	svcStuffText
	svcServerData
	svcConfigString
	svcSpawnBaseline
	svcCenterPrint
	svcDownload
	svcPlayerInfo
	svcPacketEntities
	svcDeltaPacketEntities
	svcFrame
)

// client -> server ops (enum clc_ops_e)
const (
	clcBad = iota
	clcNop
	clcMove
	clcUserinfo
	clcStringCmd
)

// print levels (q_shared.h)
const (
	printLow = iota
	printMedium
	printHigh
	printChat
)

// configstring indices (q_shared.h)
const (
	csName           = 0
	csMaxClients     = 30
	csMapChecksum    = 31
	csModels         = 32
	csPlayerSkins    = 32 + 256*4 + 256 // CS_ITEMS + MAX_ITEMS = 1312
	maxClients       = 256
	maxConfigStrings = csPlayerSkins + maxClients + 2*256 // CS_GENERAL + MAX_GENERAL
	maxEdicts        = 1024
	maxItems         = 256
)

// sound flags (sv_send.c / cl_parse.c)
const (
	sndVolume      = 1 << 0
	sndAttenuation = 1 << 1
	sndPos         = 1 << 2
	sndEnt         = 1 << 3
	sndOffset      = 1 << 4
)

// entity_state_t delta bits (qcommon/qcommon.h)
const (
	uOrigin1    = 1 << 0
	uOrigin2    = 1 << 1
	uAngle2     = 1 << 2
	uAngle3     = 1 << 3
	uFrame8     = 1 << 4
	uEvent      = 1 << 5
	uRemove     = 1 << 6
	uMoreBits1  = 1 << 7
	uNumber16   = 1 << 8
	uOrigin3    = 1 << 9
	uAngle1     = 1 << 10
	uModel      = 1 << 11
	uRenderFX8  = 1 << 12
	uEffects8   = 1 << 14
	uMoreBits2  = 1 << 15
	uSkin8      = 1 << 16
	uFrame16    = 1 << 17
	uRenderFX16 = 1 << 18
	uEffects16  = 1 << 19
	uModel2     = 1 << 20
	uModel3     = 1 << 21
	uModel4     = 1 << 22
	uMoreBits3  = 1 << 23
	uOldOrigin  = 1 << 24
	uSkin16     = 1 << 25
	uSound      = 1 << 26
	uSolid      = 1 << 27
)

// player_state_t delta flags
const (
	psMType        = 1 << 0
	psMOrigin      = 1 << 1
	psMVelocity    = 1 << 2
	psMTime        = 1 << 3
	psMFlags       = 1 << 4
	psMGravity     = 1 << 5
	psMDeltaAngles = 1 << 6
	psViewOffset   = 1 << 7
	psViewAngles   = 1 << 8
	psKickAngles   = 1 << 9
	psBlend        = 1 << 10
	psFOV          = 1 << 11
	psWeaponIndex  = 1 << 12
	psWeaponFrame  = 1 << 13
	psRDFlags      = 1 << 14
)

// player muzzle flashes (q_shared.h, MZ_*)
const (
	mzRocket   = 7
	mzSilenced = 128 // OR'd in when the shooter has the silencer
)

// player_state stats (q_shared.h)
const statChase = 16 // CS_PLAYERSKINS + slot of the chased player, or 0

// temp entity types (q_shared.h, temp_event_t) incl. Xatrix/Rogue additions
const (
	teGunshot = iota
	teBlood
	teBlaster
	teRailTrail
	teShotgun
	teExplosion1
	teExplosion2
	teRocketExplosion
	teGrenadeExplosion
	teSparks
	teSplash
	teBubbleTrail
	teScreenSparks
	teShieldSparks
	teBulletSparks
	teLaserSparks
	teParasiteAttack
	teRocketExplosionWater
	teGrenadeExplosionWater
	teMedicCableAttack
	teBFGExplosion
	teBFGBigExplosion
	teBossTport
	teBFGLaser
	teGrappleCable
	teWeldingSparks
	teGreenBlood
	teBlueHyperblaster
	tePlasmaExplosion
	teTunnelSparks
	teBlaster2
	teRailTrail2
	teFlame
	teLightning
	teDebugTrail
	tePlainExplosion
	teFlashlight
	teForceWall
	teHeatBeam
	teMonsterHeatBeam
	teSteam
	teBubbleTrail2
	teMoreBlood
	teHeatBeamSparks
	teHeatBeamSteam
	teChainfistSmoke
	teElectricSparks
	teTrackerExplosion
	teTeleportEffect
	teDballGoal
	teWidowBeamOut
	teNukeBlast
	teWidowSplash
	teExplosion1Big
	teExplosion1NP
	teFlechette
)

// serverHandler receives the interesting events out of a server message.
type serverHandler interface {
	onPrint(level int, text string)
	onCenterPrint(text string)
	onLayout(text string)
	onMuzzleFlash(entity, weapon int)
	onEntity(number int, x, y, z float64, hasX, hasY, hasZ bool)
	// onPlayerStats gets our own player_state stats: bit i set = stats[i] sent
	onPlayerStats(bits uint32, stats [32]int16)
	onStuffText(text string)
	onServerData(protocol int32, serverCount int32, gamedir string, playerNum int, levelName string)
	onConfigString(index int, value string)
	onFrame(serverFrame int32)
	onDisconnect()
	onReconnect()
}

// parseServerMessage walks one netchan payload. It returns an error if it
// hits something it can't size; everything parsed up to that point has
// already been delivered to h.
func parseServerMessage(r *msgReader, h serverHandler) error {
	for {
		if r.bad {
			return fmt.Errorf("read past end of server message")
		}
		cmd := r.Byte()
		if cmd == -1 {
			return nil // end of message
		}

		switch cmd {
		case svcNop:

		case svcDisconnect:
			h.onDisconnect()
		case svcReconnect:
			h.onReconnect()

		case svcPrint:
			level := r.Byte()
			h.onPrint(level, r.String())
		case svcCenterPrint:
			h.onCenterPrint(r.String())
		case svcStuffText:
			h.onStuffText(r.String())

		case svcServerData:
			proto := r.Long()
			count := r.Long()
			r.Byte() // attractloop
			gamedir := r.String()
			pnum := r.Short()
			level := r.String()
			h.onServerData(proto, count, gamedir, pnum, level)

		case svcConfigString:
			idx := r.Short()
			h.onConfigString(idx, r.String())

		case svcSpawnBaseline:
			bits, _ := parseEntityBits(r)
			parseDelta(r, bits)

		case svcLayout:
			h.onLayout(r.String())
		case svcInventory:
			r.Skip(2 * maxItems)

		case svcMuzzleFlash:
			// player weapon flash (p_weapon.c): sent to clients whose PVS
			// contains the shooter, i.e. who could plausibly see it
			ent := r.Short()
			h.onMuzzleFlash(ent, r.Byte())
		case svcMuzzleFlash2:
			r.Short() // monster entity
			r.Byte()  // flash number

		case svcSound:
			parseSound(r)

		case svcTempEntity:
			if err := parseTempEntity(r); err != nil {
				return err
			}

		case svcDownload:
			size := r.Short()
			r.Byte() // percent
			if size > 0 {
				r.Skip(size)
			}

		case svcFrame:
			frame := r.Long()
			r.Long() // delta frame
			r.Byte() // suppressCount (protocol 34)
			n := r.Byte()
			if n > 0 {
				r.Skip(n) // areabits
			}
			// svc_playerinfo and svc_packetentities follow as their own
			// command bytes and are handled below.
			h.onFrame(frame)

		case svcPlayerInfo:
			bits, stats := parsePlayerState(r)
			h.onPlayerStats(bits, stats)

		case svcPacketEntities, svcDeltaPacketEntities:
			if err := parsePacketEntities(r, h); err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown server command %d at byte %d", cmd, r.pos-1)
		}
	}
}

// parseEntityBits mirrors CL_ParseEntityBits.
func parseEntityBits(r *msgReader) (bits uint32, number int) {
	b := uint32(r.Byte())
	if b&uMoreBits1 != 0 {
		b |= uint32(r.Byte()) << 8
	}
	if b&uMoreBits2 != 0 {
		b |= uint32(r.Byte()) << 16
	}
	if b&uMoreBits3 != 0 {
		b |= uint32(r.Byte()) << 24
	}
	if b&uNumber16 != 0 {
		number = r.Short()
	} else {
		number = r.Byte()
	}
	return b, number
}

// parseDelta mirrors CL_ParseDelta. It skips most fields but returns the
// entity's XYZ origin when the delta carries them (coords = short / 8.0).
func parseDelta(r *msgReader, bits uint32) (x, y, z float64, hasX, hasY, hasZ bool) {
	for _, f := range []uint32{uModel, uModel2, uModel3, uModel4} {
		if bits&f != 0 {
			r.Byte()
		}
	}
	if bits&uFrame8 != 0 {
		r.Byte()
	}
	if bits&uFrame16 != 0 {
		r.Short()
	}
	skipVariable(r, bits, uSkin8, uSkin16)
	skipVariable(r, bits, uEffects8, uEffects16)
	skipVariable(r, bits, uRenderFX8, uRenderFX16)
	if bits&uOrigin1 != 0 {
		x = float64(r.Short()) / 8
		hasX = true
	}
	if bits&uOrigin2 != 0 {
		y = float64(r.Short()) / 8
		hasY = true
	}
	if bits&uOrigin3 != 0 {
		z = float64(r.Short()) / 8
		hasZ = true
	}
	for _, f := range []uint32{uAngle1, uAngle2, uAngle3} {
		if bits&f != 0 {
			r.Byte()
		}
	}
	if bits&uOldOrigin != 0 {
		r.Pos()
	}
	if bits&uSound != 0 {
		r.Byte()
	}
	if bits&uEvent != 0 {
		r.Byte()
	}
	if bits&uSolid != 0 {
		r.Short()
	}
	return
}

// skipVariable handles the "8 bit / 16 bit / both = 32 bit" field encoding
// used by skinnum, effects and renderfx.
func skipVariable(r *msgReader, bits, b8, b16 uint32) {
	switch {
	case bits&b8 != 0 && bits&b16 != 0:
		r.Long()
	case bits&b8 != 0:
		r.Byte()
	case bits&b16 != 0:
		r.Short()
	}
}

// parsePacketEntities mirrors the loop in CL_ParsePacketEntities.
func parsePacketEntities(r *msgReader, h serverHandler) error {
	for {
		bits, num := parseEntityBits(r)
		if r.bad {
			return fmt.Errorf("truncated packetentities")
		}
		if num >= maxEdicts || num < 0 {
			return fmt.Errorf("packetentities: bad entity number %d", num)
		}
		if num == 0 {
			return nil
		}
		if bits&uRemove != 0 {
			continue // removals carry no delta data
		}
		x, y, z, hasX, hasY, hasZ := parseDelta(r, bits)
		h.onEntity(num, x, y, z, hasX, hasY, hasZ)
	}
}

// parsePlayerState mirrors CL_ParsePlayerstate. Only the HUD stats are kept.
func parsePlayerState(r *msgReader) (statbits uint32, stats [32]int16) {
	flags := r.Short()
	if flags&psMType != 0 {
		r.Byte()
	}
	if flags&psMOrigin != 0 {
		r.Skip(6)
	}
	if flags&psMVelocity != 0 {
		r.Skip(6)
	}
	if flags&psMTime != 0 {
		r.Byte()
	}
	if flags&psMFlags != 0 {
		r.Byte()
	}
	if flags&psMGravity != 0 {
		r.Short()
	}
	if flags&psMDeltaAngles != 0 {
		r.Skip(6)
	}
	if flags&psViewOffset != 0 {
		r.Skip(3)
	}
	if flags&psViewAngles != 0 {
		r.Skip(6)
	}
	if flags&psKickAngles != 0 {
		r.Skip(3)
	}
	if flags&psWeaponIndex != 0 {
		r.Byte()
	}
	if flags&psWeaponFrame != 0 {
		r.Skip(1 + 3 + 3) // gunframe, gunoffset, gunangles
	}
	if flags&psBlend != 0 {
		r.Skip(4)
	}
	if flags&psFOV != 0 {
		r.Byte()
	}
	if flags&psRDFlags != 0 {
		r.Byte()
	}
	statbits = r.ULong()
	for i := 0; i < 32; i++ {
		if statbits&(1<<i) != 0 {
			stats[i] = int16(r.Short())
		}
	}
	return statbits, stats
}

// parseSound mirrors CL_ParseStartSoundPacket.
func parseSound(r *msgReader) {
	flags := r.Byte()
	r.Byte() // sound index
	if flags&sndVolume != 0 {
		r.Byte()
	}
	if flags&sndAttenuation != 0 {
		r.Byte()
	}
	if flags&sndOffset != 0 {
		r.Byte()
	}
	if flags&sndEnt != 0 {
		r.Short()
	}
	if flags&sndPos != 0 {
		r.Pos()
	}
}

// parseTempEntity mirrors CL_ParseTEnt.
func parseTempEntity(r *msgReader) error {
	t := r.Byte()
	switch t {
	case teBlood, teGunshot, teSparks, teBulletSparks, teScreenSparks,
		teShieldSparks, teShotgun, teBlaster, teGreenBlood, teBlaster2,
		teFlechette, teHeatBeamSparks, teHeatBeamSteam, teMoreBlood,
		teElectricSparks:
		r.Pos()
		r.Dir()

	case teSplash, teLaserSparks, teWeldingSparks, teTunnelSparks:
		r.Byte() // count
		r.Pos()
		r.Dir()
		r.Byte() // color

	case teBlueHyperblaster, teRailTrail, teBFGLaser, teBubbleTrail,
		teDebugTrail, teBubbleTrail2:
		r.Pos()
		r.Pos()

	case teExplosion2, teGrenadeExplosion, teGrenadeExplosionWater,
		tePlasmaExplosion, teExplosion1, teExplosion1Big, teRocketExplosion,
		teRocketExplosionWater, teExplosion1NP, teBFGExplosion,
		teBFGBigExplosion, teBossTport, tePlainExplosion, teChainfistSmoke,
		teTrackerExplosion, teTeleportEffect, teDballGoal, teNukeBlast,
		teWidowSplash:
		r.Pos()

	case teParasiteAttack, teMedicCableAttack, teHeatBeam, teMonsterHeatBeam:
		r.Short() // entity
		r.Pos()
		r.Pos()

	case teGrappleCable:
		r.Short()
		r.Pos()
		r.Pos()
		r.Pos() // offset

	case teLightning:
		r.Short() // src
		r.Short() // dest
		r.Pos()
		r.Pos()

	case teFlashlight:
		r.Pos()
		r.Short()

	case teForceWall:
		r.Pos()
		r.Pos()
		r.Byte()

	case teSteam:
		id := r.Short()
		r.Byte() // count
		r.Pos()
		r.Dir()
		r.Byte()  // color
		r.Short() // magnitude
		if id != -1 {
			r.Long() // duration
		}

	case teWidowBeamOut:
		r.Short()
		r.Pos()

	default:
		return fmt.Errorf("unknown temp entity type %d", t)
	}
	return nil
}
