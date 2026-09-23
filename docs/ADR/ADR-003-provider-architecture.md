# ADR-003 — Capability-Based External Integrations

Status: Accepted

## Context

Lernae must work with many replaceable third-party applications and metadata/storage systems. Real systems frequently provide more than one useful capability: RomM, Jellyfin or Kavita may expose inventory, metadata and other functions simultaneously.

## Decision

Core defines small **capability interfaces** such as metadata search, inventory, acquisition, storage metadata/intent, launcher, and tracker sync. A concrete external integration may implement one or several capabilities and may share one underlying client/configuration.

Core must not require every integration to inherit from or fit into one exclusive monolithic `Provider` class.

## Why

- integrations can be replaced;
- one real external system does not need redundant adapters/clients;
- core stays understandable;
- capabilities can be tested independently;
- community contributions become practical;
- personal Google Drive choices do not become universal assumptions.

## Consequences

- interfaces must stay small and capability-focused;
- not every integration needs every capability;
- provider-specific fields should not leak casually into core tables/API objects;
- concrete integrations may compose several capability implementations around one shared client.
