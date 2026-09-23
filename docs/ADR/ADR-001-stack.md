# ADR-001 — Go Server/Agent + React/TypeScript Web

Status: Proposed

## Context

Lernae needs a self-hosted server, a trusted desktop/local Agent and a modern web UI. The project will be built heavily with coding agents but maintained by a small human team, so operational simplicity matters.

## Decision

Use:

- Go for Lernae Server;
- Go for Lernae Agent;
- React + TypeScript for Web.

## Why

Go provides:

- straightforward concurrency for jobs/networking;
- strong standard library;
- easy single-binary deployment;
- simple cross-compilation path;
- lower implementation complexity than Rust for this problem.

React/TypeScript provides a mature ecosystem for rich interactive UI and makes API contracts explicit.

## Consequences

- the repository contains two primary languages;
- shared types require generation or explicit API schemas later;
- contributors need Go and Node tooling;
- we should resist adding additional backend languages without a strong reason.
