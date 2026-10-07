# MANIFEST.md — MVP Inventory Manifest

Version: 1

## Why this exists

The first Soulcalibur vertical slice must prove Lernae itself without depending on a specific RomM API version or a running external service. `ManifestInventorySource` is therefore the deterministic baseline inventory adapter.

It is intentionally small and is not the intended long-term user experience. `ManifestInventorySource` implements the shared `InventorySource` and `StorageSource` capabilities; RomM and future integrations can implement the same capabilities later.

## Minimal shape

```json
{
  "schema_version": 1,
  "assets": [
    {
      "id": "soulcalibur2-gamecube",
      "work": {
        "title": "Soulcalibur II",
        "external_ids": {"igdb": "OPTIONAL"}
      },
      "edition": {
        "platform": "gamecube",
        "format": "disc_image"
      },
      "total_size_bytes": 1450000000,
      "parts": [
        {
          "part_index": 1,
          "role": "rom",
          "filename": "Soulcalibur II.iso",
          "size_bytes": 1450000000
        }
      ],
      "locations": [
        {
          "class": "archive",
          "provider": "rclone",
          "locator": "gdrive:games/gamecube/Soulcalibur II.iso"
        }
      ]
    }
  ]
}
```

## Rules

- `schema_version` is required.
- `id` is stable within the manifest.
- `platform` is required for the Soulcalibur slice.
- `total_size_bytes` is the expected complete logical Asset size.
- `parts` contains at least one physical member; single-file games use one part.
- `locations` may contain local or remote locations, but a staging path must never be declared as ready/local inventory.
- secrets/credentials are never stored in the manifest; `provider` references configured provider state.
- paths/locators are data, never shell fragments.
- Manifest v1 has no canonical Lernae Edition ID. The Phase 0.5 adapter uses the source-local `<platform>:<format>` key when the shared inventory capability queries by Edition ID; this key is not a persisted catalog identity.

A JSON Schema is provided at `schemas/inventory-manifest.schema.json` for validation.

## Capability queries

`ManifestInventorySource.AssetsForEdition` provides the inventory query. Its `StorageSource.LocationsForAsset(ctx, assetID)` implementation returns the manifest's locations for that exact Asset as typed `domain.AssetLocation` values. The manifest's `id`, `provider`, `locator`, and `class` map to `AssetID`, `StorageProviderID`, `Locator`, and `LocationClass`; a missing Asset returns an empty slice. Manifest read or validation errors are returned to the caller. This is location metadata only: it does not access storage or transfer files.
