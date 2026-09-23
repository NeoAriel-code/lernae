# Notas de Migración y Auditoría de Identificadores (Hydra → Lernae)

Este documento registra la auditoría exhaustiva realizada sobre el repositorio y la infraestructura para el rebrand visual de **Hydra Hub** a **Lernae**.

---

## 1. Clasificación de Resultados de la Auditoría Global

La búsqueda global de términos (`hydra`, `hydra hub`, `Hydra Hub`, `HYDRA`) arrojó los siguientes componentes clasificados:

| Componente / Archivo | Tipo de Ocurrencia | Clasificación | Acción Tomada / Justificación Técnica |
| :--- | :--- | :--- | :--- |
| `config/homepage/widgets.yaml` | Texto de cabecera (`Hydra Hub`) | **A) Branding visible** | **Migrado:** Actualizado a `Lernae`. |
| `config/homepage/settings.yaml` | Título del sitio (`Hydra Hub`) | **A) Branding visible** | **Migrado:** Actualizado a `title: Lernae`. |
| `config/homepage/custom.css` | Comentarios y reglas de tema | **A / E) Branding / Doc** | **Migrado:** Sustituido por el sistema de diseño oficial de Lernae. |
| `config/homepage/images/` | Fondos de hidras mitológicas (`hydra-*.jpg`) | **A) Branding visible** | **Migrado:** Reemplazado por fondo abstracto Midnight Indigo / Violeta oficial. |
| `rclone.conf` (remote: `romm-drive`) | Nombre del remote rclone | **B) Identificador técnico** | **Preservado:** El nombre del remote en `~/.config/rclone/rclone.conf` se mantiene como `romm-drive` para no invalidar los tokens OAuth 2.0 de Google Cloud ni afectar scripts auxiliares de respaldo. |
| Google Drive Carpeta Raíz | Directorio `Hydra/` en Drive | **D) Ruta de filesystem (Cloud)** | **Migrado:** Renombrado mediante server-side move a `Lernae/` (`romm-drive:Lernae`). |
| Systemd Service | `hydra-drive.service` | **B / C) Servicio técnico** | **Migrado:** Reemplazado por `lernae-drive.service`. |
| Contenedores Docker | `hydra-*` (`hydra-kavita`, etc.) | **C) Contenedor / Red** | **Migrado:** Recreados bajo `lernae-*` (`lernae-kavita`, `lernae-readarr`, etc.). |
| Reglas de Firewall UFW | Comentarios `# Hydra ...` | **E) Comentarios / Doc** | **Preservado:** Los comentarios en iptables/ufw no tienen impacto funcional y se preservan para evitar alterar las reglas de filtrado en caliente. |
| `/home/neoariel/Games/RomM/compose.yml` | Montaje `/home/neoariel/Lernae/media/ROMs` | **D) Ruta de filesystem** | **Migrado:** Actualizado hacia `/home/neoariel/Lernae/media/ROMs`. |

---

## 2. Nombres Técnicos que Preservan Hydra y Razón Técnica

1. **Remote Rclone `romm-drive`:**
   - **Por qué se mantiene:** Es el nombre de la sección en `~/.config/rclone/rclone.conf`. Cambiar el identificador del remote requeriría reautenticar o remapear configuraciones globales del usuario. La carpeta interna a la que apunta sí fue renombrada a `Lernae`.
2. **Comentarios de UFW (Firewall):**
   - **Por qué se mantienen:** Modificar comentarios en reglas UFW existentes requeriría borrar y volver a insertar reglas en la tabla de filtrado de Linux. Como los puertos (3000, 4567, 5000, 8080, 8085, 8787, 9696) son idénticos, no se altera la tabla de firewall en caliente.
