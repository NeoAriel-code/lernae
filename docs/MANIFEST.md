# MANIFEST.md — MVP Inventory Manifest

Version: 1

## Why this exists

The first Soulcalibur vertical slice must prove Lernae itself without depending on a specific RomM API version or a running external service. `ManifestInventorySource` is therefore the deterministic baseline inventory adapter.

It is intentionally small and is not the intended long-term user experience. RomM and future inventory systems implement the same inventory capability later.

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

A JSON Schema is provided at `schemas/inventory-manifest.schema.json` for validation.
