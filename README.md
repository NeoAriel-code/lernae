# Lernae

> One library. Your universe.

Lernae es una biblioteca personal autoalojada para buscar obras, organizar universos y preparar contenido desde una sola interfaz. El Server conserva el catálogo y los Jobs; un Agent local restaura archivos y lanza Dolphin. Las integraciones especializadas son opcionales y reemplazables.

Este repositorio contiene la implementación actual de Server, Agent y Web, junto con sus pruebas y contratos públicos. La presencia de código y pruebas deterministas **no certifica aceptación en vivo de todos los proveedores**, ni reproducción de todos los medios. Los verbos **PLAY**, **WATCH**, **READ** y **LISTEN** expresan la intención del producto; el flujo de consumo implementado es el acotado de GameCube/Dolphin.

## Empezar en local

### Requisitos

- Linux para el Agent, el socket Unix y la adquisición local.
- Go **1.27+**, según [go.mod](go.mod).
- Node.js **22.12+** y npm; las dependencias Web están fijadas en el lockfile.
- Dolphin (`dolphin-emu`) para PLAY y rclone para restaurar desde un remoto configurado. No son necesarios para explorar la UI o ejecutar las pruebas deterministas ordinarias.
- Docker Compose v2 solo para los servicios externos opcionales; no sustituye el arranque de la aplicación.

Desde la raíz del repositorio:

```sh
make setup      # comprueba herramientas, descarga módulos Go y ejecuta npm ci
make configure  # guarda rutas y, opcionalmente, credenciales fuera del repositorio
make dev        # arranca Agent, Server y Web; Ctrl-C detiene los tres
```

Abre **http://127.0.0.1:5173**. Vite redirige `/api` al Server, que escucha por defecto en `127.0.0.1:8081`. La aplicación puede arrancar sin IGDB, inventario ni Agent disponible; esas capacidades aparecerán sin configurar o no disponibles, en lugar de simular resultados.

El asistente muestra los valores guardados: Enter los conserva; `default` borra una sobreescritura y vuelve al valor predeterminado. Las rutas explícitas deben ser absolutas. Para probar inventario/PLAY, configura un manifiesto propio conforme a [MANIFEST.md](docs/MANIFEST.md); [examples/manifest.json](examples/manifest.json) ilustra el contrato, no incluye archivos de contenido ni un remoto ya configurado.

## Capacidades actuales

| Área | Implementación y límite |
| --- | --- |
| Búsqueda y detalles | Agregación de IGDB (juegos), Open Library (libros) y TVMaze (TV), con caché persistente y fallos parciales. IGDB necesita credenciales propias; no hay cobertura universal de audio, anime o manga. |
| Catálogo y universos | Identidades locales, ediciones, pertenencias con evidencia y revisión manual. Relaciones, series y alias localizados; expansión Wikidata explícita y acotada con contacto válido, sin fusionar obras por títulos parecidos. |
| Inventario | Manifest aporta inventario **y** ubicaciones de almacenamiento. RomM es opcional, de solo lectura y limitado a identidad IGDB exacta/GameCube; se selecciona una fuente, sin federación. RomM solo no permite restore/PLAY. |
| Restore y PLAY | Server orquesta por UDS; Agent copia a staging, valida tamaño y promueve atómicamente antes de lanzar Dolphin con argumentos estructurados. Jobs y sesiones persisten; el arranque reconcilia operaciones interrumpidas. No se ejecuta sobre mounts remotos. |
| Adquisición | Jobs ligados a una Edition existente, descubrimiento de candidatos y selección manual inmutable. La resolución y el despacho son capacidades separadas, con reserva duradera contra repetición de efectos. |
| Adquisición local | `server-local` copia un archivo regular exacto desde raíces privadas autorizadas a staging propio del Server, verificando identidad/hash. No restaura ni lanza por Agent y no registra automáticamente un Asset. |
| Prowlarr + qBittorrent | Discovery opcional conserva referencias privadas exactas. Con ambos configurados, un resolver separado revalida la selección y el runner obtiene su torrent/magnet tras la reserva. Confirma hash y tag antes de persistir aceptación y observa progreso/completitud. Sin qBittorrent, Prowlarr queda en Discovery. No registra Assets ni inicia PLAY automáticamente. |
| Web y ajustes | UI React para búsqueda, detalles, universos y estado del sistema; preferencia de metadatos `en`, `es` u `original` (predeterminado `en`, con soporte limitado por proveedor). No equivale a un reproductor universal. |

## Configurar capacidades opcionales

```sh
make configure-igdb        # Client ID y Client Secret por entrada oculta
make configure-romm        # endpoint y Client API Token por entrada oculta
make configure-local       # adquisición Server-local; deshabilitada por defecto
make configure-prowlarr    # Discovery: habilitación, endpoint y API key oculta
make configure-qbittorrent # ejecución: habilitación, endpoint, usuario y contraseña oculta
make configure-status      # estado guardado y booleanos; sin valores privados
```

Reinicia los procesos después de cambiar rutas o credenciales. Configurar no realiza una prueba remota, y el arranque no sondea Prowlarr ni qBittorrent. `configure-status` describe lo guardado, no la disponibilidad ni todas las sobreescrituras temporales del entorno.

### Persistencia y privacidad

La configuración vive en `$XDG_CONFIG_HOME/lernae/`, o `~/.config/lernae/`:

- `settings.json`: rutas y ajustes, incluidos endpoints, usuario qBittorrent y contacto Wikidata.
- `providers.json`: credenciales separadas.
- Referencias privadas Prowlarr: registros exactos separados de las credenciales, con tokens opacos para recuperar una selección tras reiniciar.

En Linux, el directorio privado usa `0700` y los archivos privados `0600`. **No están cifrados** ni constituyen un keyring: otros procesos de tu usuario pueden leerlos. No copies estos archivos, contactos, endpoints privados o secretos al repositorio, logs públicos o incidencias. El estado de proveedores se presenta mediante booleanos, no valores.

Registra una aplicación Twitch Developer propia para IGDB; no reutilices las credenciales de la aplicación RomM. RomM requiere un token con permiso `roms.read`. Para IGDB y RomM, una pareja completa de variables de entorno sustituye el bundle guardado; si está presente cualquiera de las dos variables, incluso vacía, deben estar completas ambas. No se mezclan credenciales del archivo y del entorno.

### Rutas y sobreescrituras de entorno

En los ajustes con soporte de entorno, la prioridad es **entorno > valor guardado > predeterminado**. El asistente modifica solo la capa guardada.

| Ajuste | Variable | Valor predeterminado |
| --- | --- | --- |
| Base SQLite | `LERNAE_SERVER_DB_PATH` | `$XDG_DATA_HOME/lernae/lernae.db`, o `~/.local/share/lernae/lernae.db` |
| HTTP Server | `LERNAE_SERVER_LISTEN_ADDR` | `127.0.0.1:8081`; solo loopback literal |
| Socket compartido | `LERNAE_AGENT_SOCKET_PATH` | `$XDG_RUNTIME_DIR/lernae/agent.sock`; sin él, `$XDG_CACHE_HOME/lernae/run/agent.sock` o `~/.cache/lernae/run/agent.sock` |
| Caché Agent | `LERNAE_AGENT_CACHE_PATH` | `$XDG_CACHE_HOME/lernae/cache` o `~/.cache/lernae/cache` |
| Staging Agent | `LERNAE_AGENT_STAGING_PATH` | `<caché Agent>/.staging` |
| Fuente de inventario | `LERNAE_INVENTORY_SOURCE` | `manifest`; alternativa explícita `romm` |
| Manifiesto | `LERNAE_INVENTORY_MANIFEST_PATH` | Sin configurar |
| Credenciales IGDB | `LERNAE_IGDB_CLIENT_ID`, `LERNAE_IGDB_CLIENT_SECRET` | Bundle guardado o ausente |
| Credenciales RomM | `LERNAE_ROMM_BASE_URL`, `LERNAE_ROMM_CLIENT_API_TOKEN` | Bundle guardado o ausente |
| Contacto Wikidata | `LERNAE_WIKIDATA_CONTACT_EMAIL` | Ausente; resolver opcional deshabilitado |

Prowlarr, qBittorrent y las raíces de adquisición local usan la configuración persistida; no requieren exportar credenciales. Un contacto Wikidata no vacío pero inválido deshabilita solo ese resolver. El contacto se introduce como texto visible y se guarda sin cifrar; `configure-status` no lo imprime.

El Agent crea el directorio del socket con permisos privados, rechaza directorios escribibles por grupo/otros y protege el socket con `0600`. Caché y staging de restore deben permitir promoción atómica en el mismo filesystem. Las raíces de adquisición local del Server son un contrato distinto: directorios existentes, privados y separados, sin reutilizar la caché del Agent.

### Límite de ejecución Prowlarr/qBittorrent

El runner exige **qBittorrent `v5.0.0` y Web API `2.11.2`** en tiempo de ejecución. Otros valores fallan de forma segura; no hay adaptación automática a versiones. Se admite el transporte torrent acotado y la redirección magnet oficial soportada, no navegación arbitraria ni sustitución de una release cuando falla su localizador.

Una selección queda reservada de forma duradera antes de obtener el contenido remoto y antes de `torrents/add`. Un add ambiguo no se repite; no se adoptan ni etiquetan torrents ajenos. La aceptación requiere observar el hash esperado junto con el tag único de esa ejecución. La observación comprueba estado y contadores del cliente, no una verificación independiente del archivo descargado ni promoción a inventario. Reiniciar no reproduce el despacho ni reanuda automáticamente la observación: los Jobs activos se reconcilian como interrumpidos. Detener Server no elimina ni cancela automáticamente el torrent en el cliente.

## Desarrollo y verificación

```sh
make help
make lint                            # gofmt, go vet y ESLint
make typecheck                       # TypeScript
make test                            # pruebas Go deterministas
npm --prefix apps/web run test -- --run # Vitest sin modo interactivo
make build                           # build/lernae-server, build/lernae-agent y apps/web/dist
```

Para la adquisición y su configuración:

```sh
go test ./internal/acquisition ./internal/config ./cmd/provider-config ./cmd/server ./internal/api
```

`make agent`, `make server` y `make web` arrancan componentes por separado. Inicia Agent antes que Server si quieres que el diagnóstico inicial lo reporte conectado. `GET /api/v1/health` comprueba liveness y `GET /api/v1/system/status` informa sobre Server, base y Agent; un Agent offline es un estado de componente, no un fallo de liveness.

Las pruebas ordinarias usan fixtures/fakes y no acreditan aceptación externa. Los smoke tests opt-in requieren contenido legítimo y configuración local explícita; no forman una garantía de proveedores reales ni deben publicarse con credenciales. Los artefactos de compilación, dependencias, descargas, bases y cachés no se versionan.

## Servicios externos con Compose

[compose.yml](compose.yml) es una configuración opcional para Homepage, Kavita, Suwayomi, Readarr, Prowlarr y qBittorrent. **No incluye Server, Agent, Web ni RomM** y no implementa una instalación gestionada completa de proveedores.

```sh
docker compose -f compose.yml config --quiet
# Solo si deseas iniciar el panel independiente:
docker compose -f compose.yml up -d lernae-homepage
```

Homepage queda en **http://localhost:3000**. Los enlaces de [config/homepage/services.yaml](config/homepage/services.yaml) son muestras locales, no evidencia de servicios instalados. Sus recursos gráficos se conservan en `config/homepage/images/`; Homepage es un panel de sistema independiente del frontend React.

| Variable Compose | Predeterminado | Uso |
| --- | --- | --- |
| `LERNAE_DATA_DIR` | `./data` | Configuración privada de los servicios externos |
| `LERNAE_MEDIA_DIR` | `./media` | Bibliotecas; crea `Mangas`, `Libros` y `Novelas` antes de iniciar sus servicios |
| `LERNAE_DOWNLOADS_DIR` | `./downloads` | Descargas locales compartidas por qBittorrent y Readarr |
| `PUID`, `PGID` | `1000`, `1000` | Ajusta al usuario/grupo propietario de los directorios |
| `TZ` | `Etc/UTC` | Zona horaria de los servicios |
| `HOMEPAGE_ALLOWED_HOSTS` | `localhost:3000,127.0.0.1:3000` | Hosts admitidos por Homepage |
| `LERNAE_QBITTORRENT_IMAGE` | `ghcr.io/linuxserver/qbittorrent:latest` | Selecciona una imagen que entregue las versiones exactas exigidas por el runner; `latest` no las garantiza |

Las rutas relativas se resuelven desde el proyecto Compose; no hay paths de una máquina personal ni credenciales incorporadas. qBittorrent debe guardar en `/downloads`, montado a almacenamiento **local** del host. El runner no envía un `savepath`, no necesita compartir la ruta absoluta con Server y no importa ese directorio como Asset/caché Agent. No lo sustituyas por un mount remoto de acceso aleatorio.

Los paneles HTTP se publican solo en loopback; el puerto de pares BitTorrent `6881` sigue siendo de red. Homepage monta el socket Docker: un mount `ro` no convierte su API en inocua, así que úsalo solo en un entorno de confianza. No expongas estos paneles ni Server a Internet sin una decisión de seguridad separada. Las imágenes y servicios externos requieren validación propia; este Compose no prueba compatibilidad en vivo.

## Documentación y límites

- [Producto](docs/PRODUCT.md): visión y principios, no promesa de que cada función esté terminada.
- [Arquitectura](docs/ARCHITECTURE.md) y [ADR](docs/ADR/): límites y decisiones aceptadas.
- [Modelo de datos](docs/DATA_MODEL.md): Universe → Work → Edition → Asset y relaciones.
- [MVP](docs/MVP.md): criterios de la prueba GameCube/Dolphin, no acta de aceptación.
- [Manifiesto](docs/MANIFEST.md), [esquemas](schemas/) y [ejemplos](examples/).
- [Contribución](docs/CONTRIBUTING.md) y [reglas para agentes](AGENTS.md).

No hay autenticación pública, multi-Agent, reproducción universal, sincronización completa con trackers, marketplace de plugins ni garantía Windows/macOS. Los registros de planificación y auditoría locales no forman parte de la documentación pública ni son necesarios para seguir estas instrucciones.

Lernae coordina fuentes lícitas y herramientas configuradas por el usuario: copias personales, bibliotecas existentes y sistemas autorizados. Core no incorpora catálogos infractores, evasión de DRM, credenciales ni atajos para eludir condiciones de proveedores.
