# JD-форк vault-mcp-server

Внутренний read-only форк [hashicorp/vault-mcp-server](https://github.com/hashicorp/vault-mcp-server) — из него собирается образ сервиса `mcp-vault` на [хосте MCP-коннекторов gvm25](https://jd-infra-docs.lpr.jet.msk.su/llm/mcp-hub/). База — upstream `main` (v0.2.0 + 40 коммитов), поверх — три патча; ветка `main` этого репозитория = база + все три.

## Патчи

| Патч | Что меняет | Upstream PR |
| --- | --- | --- |
| `feat(kv): include KV v2 metadata in read_secret response` | `read_secret` возвращает `{data, metadata}` вместо голого `data`: метаданные KV v2 (`custom_metadata` с `description`/`usage`, версия, таймстемпы) больше не срезаются — агент видит описание секрета из Vault UI → Metadata | [#125](https://github.com/hashicorp/vault-mcp-server/pull/125) |
| `feat(tools): gate mutating tools behind ENABLE_VAULT_OPERATIONS` | Реализован задокументированный, но не работавший в upstream `ENABLE_VAULT_OPERATIONS`: write-тулзы (`write_secret`, `delete_secret`, `create_mount`/`delete_mount`, PKI-write) регистрируются только при `ENABLE_VAULT_OPERATIONS=true`. Дефолт — read-only: этих тулз нет в `tools/list` | [#126](https://github.com/hashicorp/vault-mcp-server/pull/126) |
| `feat(kv): add write_secret_metadata to describe KV v2 secrets` | Новая тулза `write_secret_metadata(mount, path, custom_metadata)` пишет `custom_metadata` KV v2 — без неё агент заводит секрет, но не может проставить ему `description`/`usage`, обязательные по правилам JD. Пишет через HTTP PATCH (JSON merge patch), поэтому переданные ключи обновляются, остальные ключи `custom_metadata` и поля `max_versions`/`cas_required`/`delete_version_after` остаются как были; `null` в значении удаляет ключ. Тулза мутирующая — регистрируется только при `ENABLE_VAULT_OPERATIONS=true` | ветка есть, PR не открыт |

Ветки `pr/read-secret-metadata`, `pr/enable-vault-operations` и `pr/write-secret-metadata` — те же патчи, по одному на ветку; из первых двух открыты upstream-PR. Примет upstream все три — форк можно сворачивать, вернувшись на официальный образ.

Если сервер запущен с `ENABLE_VAULT_OPERATIONS=true`, политике Vault для `write_secret_metadata` нужна capability **`patch`** на `<mount>/metadata/*` — одних `create`/`update` мало: PATCH и POST на этом пути Vault различает, а POST здесь не годится, он заменяет объект метаданных целиком.

## Сборка и деплой образа

Образ собирается минимальным `Dockerfile.jd` из кросс-компилированного бинаря
(scratch + ca-certs) и публикуется в Nexus. Официальный `--target=release-default`
в нашем контуре **не годится** — его `certbuild`-база
`docker.mirror.hashicorp.services/alpine` с gvm25 недоступна; `Dockerfile.jd` берёт
`alpine` из Docker Hub.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" \
  -o vault-mcp-server ./cmd/vault-mcp-server
docker build -f Dockerfile.jd --platform=linux/amd64 \
  -t nexus.lpr.jet.msk.su:5007/jd-docker/vault-mcp-server:0.2.0-jd-ro2 .
docker push nexus.lpr.jet.msk.su:5007/jd-docker/vault-mcp-server:0.2.0-jd-ro2
```

Сборку удобно делать прямо на gvm25 (нативный amd64): залить `Dockerfile.jd` +
бинарь и `docker build` — так и собран текущий `0.2.0-jd-ro2`.

Схема тегов — `<upstream-версия>-jd-roN`, `N` растёт с каждой ревизией форка. Деплой: тег закреплён в `/var/docker/compose/mcp/docker-compose.yml` на gvm25 → `docker-compose pull && docker-compose up -d`.
