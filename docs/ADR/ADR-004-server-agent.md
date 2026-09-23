# ADR-004 — Separate Server and Local Agent Responsibilities

Status: Accepted

## Context

Lernae's web/server component needs to orchestrate local GUI applications such as Dolphin and prepare files on the machine that will consume them. A self-hosted server cannot safely assume it shares a desktop session or filesystem with that machine. The Foundation audit also identified ambiguity over whether Server or Agent physically executes remote restores.

## Decision

Create a Lernae Agent responsible for trusted machine-local actions, while Server owns catalog/orchestration/user state and durable Job state.

**Server orchestrates; Agent executes.**

For MVP, Server and Agent run on the same Linux workstation and communicate over a Unix Domain Socket (UDS) protected by user-only filesystem permissions. Future remote Agent networking requires a new ADR.

## Agent responsibilities

- report capabilities;
- own/manage local staging and cache paths;
- execute delegated storage restore/archive operations (including rclone);
- verify completion and atomically promote staged files into ready cache;
- launch allowed applications with structured arguments;
- track local sessions;
- report transfer/session progress to Server.

## Server responsibilities

- user-facing API;
- search/resolution/catalog;
- capability/integration registry;
- durable Jobs/orchestration;
- storage intent and remote-location metadata;
- persistent personal state.

## Consequences

- requires a defined Server↔Agent protocol;
- adds a component early;
- prevents restoring content onto the wrong machine/filesystem;
- avoids an unauthenticated localhost launch API in MVP;
- prevents a much harder architectural split later;
- enables future multi-device playback without changing the core product model.
