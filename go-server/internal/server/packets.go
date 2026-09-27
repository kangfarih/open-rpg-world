package server

import "rpg-world-server/internal/protocol"

// Packet shims re-exporting internal/protocol.
// Behavior-frozen: all logic lives in internal/protocol; this file only
// aliases names used by main.go and m5.go..m13.go.

const (
	PacketConnected    = protocol.PacketConnected
	PacketHandshake    = protocol.PacketHandshake
	PacketLogin        = protocol.PacketLogin
	PacketWelcome      = protocol.PacketWelcome
	PacketMap          = protocol.PacketMap
	PacketSpawn        = protocol.PacketSpawn
	PacketList         = protocol.PacketList
	PacketWho          = protocol.PacketWho
	PacketEquipment    = protocol.PacketEquipment
	PacketReady        = protocol.PacketReady
	PacketSync         = protocol.PacketSync
	PacketMovement     = protocol.PacketMovement
	PacketTeleport     = protocol.PacketTeleport
	PacketDespawn      = protocol.PacketDespawn
	PacketTarget       = protocol.PacketTarget
	PacketCombat       = protocol.PacketCombat
	PacketAnimation    = protocol.PacketAnimation
	PacketPoints       = protocol.PacketPoints
	PacketNetwork      = protocol.PacketNetwork
	PacketChat         = protocol.PacketChat
	PacketCommand      = protocol.PacketCommand
	PacketContainer    = protocol.PacketContainer
	PacketAbility      = protocol.PacketAbility
	PacketQuest        = protocol.PacketQuest
	PacketAchievement  = protocol.PacketAchievement
	PacketNotification = protocol.PacketNotification
	PacketBlink        = protocol.PacketBlink
	PacketHeal         = protocol.PacketHeal
	PacketExperience   = protocol.PacketExperience
	PacketDeath        = protocol.PacketDeath
	PacketMusic        = protocol.PacketMusic
	PacketNPC          = protocol.PacketNPC
	PacketRespawn      = protocol.PacketRespawn
	PacketTrade        = protocol.PacketTrade
	PacketEnchant      = protocol.PacketEnchant
	PacketGuild        = protocol.PacketGuild
	PacketPointer      = protocol.PacketPointer
	PacketPVP          = protocol.PacketPVP
	PacketPoison       = protocol.PacketPoison
	PacketWarp         = protocol.PacketWarp
	PacketStore        = protocol.PacketStore
	PacketOverlay      = protocol.PacketOverlay
	PacketCamera       = protocol.PacketCamera
	PacketBubble       = protocol.PacketBubble
	PacketSkill        = protocol.PacketSkill
	PacketUpdate       = protocol.PacketUpdate
	PacketMinigame     = protocol.PacketMinigame
	PacketEffect       = protocol.PacketEffect
	PacketFriends      = protocol.PacketFriends
	PacketFocus        = protocol.PacketFocus
	PacketRank         = protocol.PacketRank
	PacketExamine      = protocol.PacketExamine
	PacketPlayer       = protocol.PacketPlayer
	PacketRelay        = protocol.PacketRelay
	PacketCrafting     = protocol.PacketCrafting
	PacketInterface    = protocol.PacketInterface
	PacketLootBag      = protocol.PacketLootBag
	PacketCountdown    = protocol.PacketCountdown
	PacketPet          = protocol.PacketPet
	PacketResource     = protocol.PacketResource
	PacketAdminSync    = protocol.PacketAdminSync
)

const (
	EntityPlayer     = protocol.EntityPlayer
	EntityNPC        = protocol.EntityNPC
	EntityItem       = protocol.EntityItem
	EntityMob        = protocol.EntityMob
	EntityChest      = protocol.EntityChest
	EntityProjectile = protocol.EntityProjectile
	EntityLootBag    = protocol.EntityLootBag
	EntityEffect     = protocol.EntityEffect
	EntityTree       = protocol.EntityTree
	EntityRock       = protocol.EntityRock
	EntityForaging   = protocol.EntityForaging
	EntityFishSpot   = protocol.EntityFishSpot
)

const OrientationDown = protocol.OrientationDown

const (
	MovementRequest = protocol.MovementRequest
	MovementStarted = protocol.MovementStarted
	MovementStep    = protocol.MovementStep
	MovementStop    = protocol.MovementStop
	MovementMove    = protocol.MovementMove
	MovementFollow  = protocol.MovementFollow
	MovementEntity  = protocol.MovementEntity
)

const (
	ActionIdle   = protocol.ActionIdle
	ActionAttack = protocol.ActionAttack
)

const (
	EquipmentHelmet     = protocol.EquipmentHelmet
	EquipmentPendant    = protocol.EquipmentPendant
	EquipmentArrows     = protocol.EquipmentArrows
	EquipmentChestplate = protocol.EquipmentChestplate
	EquipmentWeapon     = protocol.EquipmentWeapon
	EquipmentShield     = protocol.EquipmentShield
	EquipmentRing       = protocol.EquipmentRing
	EquipmentArmourSkin = protocol.EquipmentArmourSkin
	EquipmentWeaponSkin = protocol.EquipmentWeaponSkin
	EquipmentLegplates  = protocol.EquipmentLegplates
	EquipmentCape       = protocol.EquipmentCape
	EquipmentBoots      = protocol.EquipmentBoots
)

const (
	EquipmentBatch   = protocol.EquipmentBatch
	EquipmentEquip   = protocol.EquipmentEquip
	EquipmentUnequip = protocol.EquipmentUnequip
	EquipmentStyle   = protocol.EquipmentStyle
)

const ModulesEquipmentCount = protocol.ModulesEquipmentCount

const (
	ResourceStateDefault  = protocol.ResourceStateDefault
	ResourceStateDepleted = protocol.ResourceStateDepleted
)

const (
	TargetTalk   = protocol.TargetTalk
	TargetAttack = protocol.TargetAttack
	TargetObject = protocol.TargetObject
)

const (
	CombatHit    = protocol.CombatHit
	CombatFinish = protocol.CombatFinish
	CombatSync   = protocol.CombatSync
)

const (
	HitsNormal     = protocol.HitsNormal
	HitsPoison     = protocol.HitsPoison
	HitsCritical   = protocol.HitsCritical
	HitsHeal       = protocol.HitsHeal
	HitsStun       = protocol.HitsStun
	HitsFreezing   = protocol.HitsFreezing
	HitsBurning    = protocol.HitsBurning
	HitsTerror     = protocol.HitsTerror
	HitsExplosive  = protocol.HitsExplosive
	HitsProfession = protocol.HitsProfession
)

const (
	EffectBoulder          = protocol.EffectBoulder
	EffectHealing          = protocol.EffectHealing
	EffectFireball         = protocol.EffectFireball
	EffectDefenseBuff      = protocol.EffectDefenseBuff
	EffectStrengthSuperBuf = protocol.EffectStrengthSuperBuf
)

const (
	EffectAdd    = protocol.EffectAdd
	EffectRemove = protocol.EffectRemove
)

const (
	OrientationLeft  = protocol.OrientationLeft
	OrientationRight = protocol.OrientationRight
)

const (
	ContainerBatch         = protocol.ContainerBatch
	ContainerAdd           = protocol.ContainerAdd
	ContainerRemove        = protocol.ContainerRemove
	ContainerSelect        = protocol.ContainerSelect
	ContainerSwap          = protocol.ContainerSwap
	ContainerTypeBank      = protocol.ContainerTypeBank
	ContainerTypeInventory = protocol.ContainerTypeInventory
)

const (
	ExperienceSync  = protocol.ExperienceSync
	ExperienceSkill = protocol.ExperienceSkill
)

const (
	StoreOpen   = protocol.StoreOpen
	StoreClose  = protocol.StoreClose
	StoreBuy    = protocol.StoreBuy
	StoreSell   = protocol.StoreSell
	StoreUpdate = protocol.StoreUpdate
	StoreSelect = protocol.StoreSelect
)

const (
	NPCTalk    = protocol.NPCTalk
	NPCStore   = protocol.NPCStore
	NPCBank    = protocol.NPCBank
	NPCEnchant = protocol.NPCEnchant
)

const (
	TradeRequest = protocol.TradeRequest
	TradeAdd     = protocol.TradeAdd
	TradeRemove  = protocol.TradeRemove
	TradeAccept  = protocol.TradeAccept
	TradeClose   = protocol.TradeClose
	TradeOpen    = protocol.TradeOpen
)

const (
	EnchantSelect  = protocol.EnchantSelect
	EnchantConfirm = protocol.EnchantConfirm
)

const (
	CraftingOpen   = protocol.CraftingOpen
	CraftingSelect = protocol.CraftingSelect
	CraftingCraft  = protocol.CraftingCraft
)

const (
	SkillCooking   = protocol.SkillCooking
	SkillSmithing  = protocol.SkillSmithing
	SkillCraftingS = protocol.SkillCraftingS
	SkillChiseling = protocol.SkillChiseling
	SkillFletching = protocol.SkillFletching
	SkillSmelting  = protocol.SkillSmelting
	SkillLoitering = protocol.SkillLoitering
	SkillAlchemy   = protocol.SkillAlchemy
)

const NotificationText = protocol.NotificationText

const (
	ModulesInventorySize = protocol.ModulesInventorySize
	ModulesBankSize      = protocol.ModulesBankSize
	ModulesMaxStack      = protocol.ModulesMaxStack
	StoreRefreshInterval = protocol.StoreRefreshInterval
)

const (
	SkillBatch  = protocol.SkillBatch
	SkillUpdate = protocol.SkillUpdate
)

const (
	LootBagOpen  = protocol.LootBagOpen
	LootBagTake  = protocol.LootBagTake
	LootBagClose = protocol.LootBagClose
)

type (
	Enchantment        = protocol.Enchantment
	Enchantments       = protocol.Enchantments
	HitData            = protocol.HitData
	EntityDisplayInfo  = protocol.EntityDisplayInfo
	EntityData         = protocol.EntityData
	ResourceEntityData = protocol.ResourceEntityData
	PlayerData         = protocol.PlayerData
	HandshakeData      = protocol.HandshakeData
	RegionTile         = protocol.RegionTile
)

type (
	clientMovement         = protocol.ClientMovement
	serverMovement         = protocol.ServerMovement
	respawnData            = protocol.RespawnData
	teleportData           = protocol.TeleportData
	equipBatchData         = protocol.EquipBatchData
	clientEquipment        = protocol.ClientEquipment
	combatData             = protocol.CombatData
	pointsData             = protocol.PointsData
	effectData             = protocol.EffectData
	healData               = protocol.HealData
	despawnData            = protocol.DespawnData
	animationData          = protocol.AnimationData
	resourceData           = protocol.ResourceData
	storeItemData          = protocol.StoreItemData
	storePacketData        = protocol.StorePacketData
	npcPacketData          = protocol.NpcPacketData
	notificationPacketData = protocol.NotificationPacketData
	clientStore            = protocol.ClientStore
	clientContainer        = protocol.ClientContainer
	slotData               = protocol.SlotData
	containerBatch         = protocol.ContainerBatchPayload
	containerData          = protocol.ContainerData
	experienceData         = protocol.ExperienceData
	skillData              = protocol.SkillData
)

func enchAny(e Enchantments) map[string]any { return protocol.EnchAny(e) }

func floatp(v float64) *float64 { return protocol.Floatp(v) }

func pkt(id int, data any) []any { return protocol.Pkt(id, data) }

func pktOp(id, opcode int, data any) []any { return protocol.PktOp(id, opcode, data) }

func mapPkt(base64 string, bufSize int) []any { return protocol.MapPkt(base64, bufSize) }
