import { useTranslation } from "react-i18next";
import type { CapabilityEvidence, ProviderCapabilities, ProviderProfilesCatalog } from "./types";

/** What goes in and what comes out, drawn from the mapping the server serves.
 *
 * The modalities are written out rather than drawn. They were five hand-made
 * 16px glyphs, and at that size the picture stopped being a word: the image mark
 * (a frame, a mountain and a sun) turned to mush, and the fetched-image mark (a
 * frame and an arc) read as a bracket. An operator had to hover each one to
 * learn what it meant, which is a legend with extra steps — and in a
 * Chinese-first console two characters are both shorter and unambiguous.
 *
 * The evidence behind a modality survives the change: verified is set in a
 * heavier weight than declared, so it is not carried by colour alone and still
 * separates in a monochrome rendering.
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
    const name = t(`deployments.modalityNames.${row.modality}`);
    const label = t("deployments.modalityMark", {
      direction: t(`deployments.modalityDirections.${row.direction}`),
      modality: name,
      evidence: t(`deployments.evidenceValues.${row.evidence}`),
    });
    return (
      <span className="modality-mark" key={`${row.direction}:${row.modality}`} data-evidence={row.evidence} role="img" aria-label={label} title={label}>
        {name}
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
