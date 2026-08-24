import { useTranslation } from "react-i18next";
import type { CapabilityEvidence, ProviderCapabilities, ProviderProfilesCatalog } from "./types";

/** What goes in and what comes out, drawn from the mapping the server serves.
 *
 * The mapping is not derived here on purpose. Halro's capabilities are
 * operations and protocol features; the input/output view a model catalogue
 * shows is a different vocabulary, and the translation between them is not
 * obvious — transcriptions is an operation whose input is audio, speech is
 * text-to-audio and so has no input row of its own. Writing that translation a
 * second time in the browser is how the console and the server drift apart, so
 * `capability_modalities` arrives with the profile bundle and this file only
 * renders it.
 *
 * A capability the mapping does not mention is not missing from this row by
 * accident: `non_modal_capabilities` names the ones that describe the protocol
 * rather than the data, and they are shown as a count beside the marks instead.
 */

const glyphs: Record<string, { path: string; viewBox?: string }> = {
  // Serif "T": the letterform reads as text at 16px where a page glyph does not.
  text: { path: "M3 4h10M8 4v9" },
  image: { path: "M2.5 3.5h11v9h-11zM4.5 10.5l2.5-3 2 2.5 1.5-1.5 1 2M5.5 6.25a.75.75 0 1 0 0-.01" },
  fetched_image: { path: "M2.5 3.5h8v9h-8zM12 4.5c1.8.8 2.8 2.2 2.8 3.5S13.8 11.2 12 12" },
  audio: { path: "M8 2.5a1.6 1.6 0 0 1 1.6 1.6v3.6a1.6 1.6 0 0 1-3.2 0V4.1A1.6 1.6 0 0 1 8 2.5zM4.4 7.4a3.6 3.6 0 0 0 7.2 0M8 11v2.5" },
  embedding: { path: "M2.5 8h1.6M6.4 8H8M10.4 8H12M13.9 8h.6" },
};

export function modalityRows(
  catalog: ProviderProfilesCatalog | undefined,
  capabilities: ProviderCapabilities,
  evidence: Record<string, CapabilityEvidence> | undefined,
) {
  if (!catalog?.capability_modalities) return [];
  return catalog.capability_modalities.flatMap((row) => {
    const expressed = row.capabilities.filter((name) => capabilities[name as keyof ProviderCapabilities] === true);
    if (!expressed.length) return [];
    // Solid when something verified it, outline when it was only declared. A
    // modality expressed by two capabilities takes the stronger of the two:
    // the row is claiming the modality, not either capability.
    const verified = expressed.some((name) => evidence?.[name] === "verified");
    return [{ direction: row.direction, modality: row.modality, evidence: verified ? "verified" : "declared" }];
  });
}

export function ModalityMarks({
  catalog, capabilities, evidence,
}: {
  catalog: ProviderProfilesCatalog | undefined;
  capabilities: ProviderCapabilities;
  evidence?: Record<string, CapabilityEvidence>;
}) {
  const { t } = useTranslation();
  const rows = modalityRows(catalog, capabilities, evidence);
  const inputs = rows.filter((row) => row.direction === "input");
  const outputs = rows.filter((row) => row.direction === "output");
  if (!inputs.length && !outputs.length) {
    return <span className="modality-marks-empty">{t("deployments.modalitiesUnknown")}</span>;
  }
  const mark = (row: (typeof rows)[number]) => {
    const glyph = glyphs[row.modality];
    const label = t("deployments.modalityMark", {
      direction: t(`deployments.modalityDirections.${row.direction}`),
      modality: t(`deployments.modalityNames.${row.modality}`),
      evidence: t(`deployments.evidenceValues.${row.evidence}`),
    });
    return (
      <span className="modality-mark" key={`${row.direction}:${row.modality}`} data-evidence={row.evidence} title={label}>
        <svg viewBox="0 0 16 16" role="img" aria-label={label} focusable="false">
          <path
            d={glyph?.path ?? "M3 8h10"}
            fill="none"
            stroke="currentColor"
            strokeWidth={row.evidence === "verified" ? 1.9 : 1.1}
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </span>
    );
  };
  return (
    <span className="modality-marks">
      {inputs.map(mark)}
      {/* Decorative: the direction is already named inside each mark's label,
          so an assistive reader that announced the arrow too would hear the
          same fact twice. */}
      <span className="modality-arrow" aria-hidden="true">→</span>
      {outputs.length ? outputs.map(mark) : <span className="modality-marks-empty">{t("deployments.modalitiesUnknown")}</span>}
    </span>
  );
}
