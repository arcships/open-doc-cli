# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial release of opendoc: one-way mirroring of authorized Notion and
  Feishu/Lark documents into a local, read-only Markdown tree.
  - Incremental sync with full-reconciliation rounds, delete/move tracking,
    and a permission-flap guard.
  - Content-addressed asset pool, two-phase internal-link rewriting, and an
    auto-generated `INDEX.md` library map.
  - Embedded Feishu engine (no external CLI or Node runtime required);
    Notion via the official API with a read-only integration token.
  - Agent-first CLI: `init` / `sync` / `status` / `doctor` / `resolve` /
    `schedule`, with structured `--json` output, deterministic exit codes,
    and stable doctor failure codes.
  - Claude Code plugin (`skill/`) that wraps the engine and teaches agents
    how to retrieve from the mirror.

### Changed

- The plugin package (`skill/`) now supports Codex alongside Claude Code:
  dual manifests (`.claude-plugin/plugin.json` + `.codex-plugin/plugin.json`)
  and the skill moved to the standard nested layout (`skills/opendoc/SKILL.md`).
  Distribution is now plugin-marketplace-only via the separate catalog repo
  `arcships/plugins` (git-subdir entries that sparse-fetch just `skill/`, so
  installs never pull this repo's source); the catalogs kept at this repo's
  root are the dev-only `arcships-dev` marketplace installing from the working
  tree. The `~/.claude/skills` symlink install path is gone, and
  `scripts/build-skill.sh` only builds the binary. CI/release now enforce that
  both manifest versions stay in sync and match the release tag.
