import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Loading, Modal } from "../components";
import { predatesProviderIdentifiers } from "../failure";

// Everything known about one failure, in one place.
//
// A drawer, not an expanding row and not a centred dialog. What is read here is
// a captured request body — tall and wide — and growing a table row to hold one
// moves every row below it while the operator is reading. The drawer is the
// console's full-height surface: a sticky header, a scrolling body, and a
// footer pinned to the bottom edge, so the way out is in the same place whether
// the record is four facts or a payload that scrolls for a minute.
//
// It is shared by the failed-request list and the attempt log because the two
// answer the same question about different rows. Each page supplies its own
// facts; nothing about the shape, the payload gate or the audit story is
// written twice.

export type FailureFact = { label: string; value?: string; code?: boolean; emphasis?: boolean };

export function FailureDetailDrawer({ title, facts, advice, identifiersUnrecorded, links, requestID, onClose }: {
  title: string;
  facts: FailureFact[];
  advice?: string;
  // True when the record predates the fields carrying the upstream's own
  // identifiers. A blank code there and a blank code on a record that had none
  // are different answers, and one rendering for both talks the operator out of
  // chasing an identifier that does exist upstream.
  identifiersUnrecorded?: boolean;
  links?: React.ReactNode;
  // The request whose captured payload this failure belongs to. Omitted where
  // there can be no payload — a refusal that never reached an upstream — so the
  // panel is absent rather than offering an audited read of nothing.
  requestID?: string;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Modal drawer title={title} onClose={onClose}>
      {/* The modal insets a <form> child and nothing else, so a body built out
          of a list and a couple of sections would otherwise run flush against
          all four borders. */}
      <div className="failure-detail-body">
        <dl className="failure-facts">
          {/* Keyed by position: the list is assembled fresh on every render in a
              fixed order, and a label is a translated string. */}
          {facts.map((fact, index) => <Fact key={index} {...fact} />)}
        </dl>

        {/* What to do next, kept apart from the facts: one is a record, the
            other is advice, and running them together makes the advice read as
            something the ledger said. */}
        {advice && <p className="failure-advice">{advice}</p>}
        {identifiersUnrecorded && (
          <p className="failure-advice muted">{t("usage.identifiersNotRecorded")}</p>
        )}
        {links && <p className="failure-links">{links}</p>}

        {requestID && <CapturedPayload requestID={requestID} />}
      </div>

      {/* A sibling of the body, not the last thing inside it: the drawer pins
          this to its bottom edge, so the way out does not move with the length
          of what is being read. */}
      <div className="form-actions failure-detail-actions">
        {/* No data-modal-initial: without it the Modal focuses its own
            container, so the first thing announced is the drawer's title
            rather than the way out of it. */}
        <button type="button" className="button ghost" data-modal-close>
          {t("common.close")}
        </button>
      </div>
    </Modal>
  );
}

// One fact, or nothing. A label with an em dash under it is a row of furniture
// that tells the reader the field exists and says nothing; leaving it out says
// the same thing in no space at all.
function Fact({ label, value, code = false, emphasis = false }: FailureFact) {
  if (!value) return null;
  return (
    <div className={`failure-fact${emphasis ? " emphasis" : ""}`}>
      <dt>{label}</dt>
      <dd>{code ? <code>{value}</code> : value}</dd>
    </div>
  );
}

// providerIdentifierFacts turns the upstream's own identifiers into facts, and
// reports whether the record simply predates them. Shared so the failed-request
// row and the attempt row cannot disagree about when a blank means "none" and
// when it means "not kept".
export function providerIdentifierFacts(
  t: (key: string, values?: Record<string, unknown>) => string,
  failure: { provider_code?: string; provider_request_id?: string; failure_phase?: string } | undefined,
): { facts: FailureFact[]; unrecorded: boolean } {
  if (!failure || predatesProviderIdentifiers(failure)) {
    return { facts: [], unrecorded: Boolean(failure) };
  }
  return {
    unrecorded: false,
    facts: [
      { label: t("usage.failures.providerCodeLabel"), value: failure.provider_code, code: true },
      { label: t("usage.failures.providerRequestLabel"), value: failure.provider_request_id, code: true },
    ],
  };
}

// What the failed call carried, fetched only when an operator asks for it.
//
// It is behind a click rather than loaded with the drawer for three reasons
// that point the same way: it is the only thing here holding material a caller
// wrote, the server audits every read of it, and loading it on open would file
// an audit record for every failure an operator merely looked at.
//
// It is not retained — `gcTime: 0` evicts it as soon as the drawer unmounts,
// which is the browser-side version of the storage decision this feature was
// careful about. It does live in the query cache while the drawer is open;
// there is nowhere else for it to be.
function CapturedPayload({ requestID }: { requestID: string }) {
  const { t } = useTranslation();
  const [requested, setRequested] = useState(false);
  const payload = useQuery({
    queryKey: ["usage-failure-payload", requestID],
    queryFn: () => api.usageFailurePayload(requestID),
    enabled: requested,
    gcTime: 0,
    staleTime: 0,
    retry: false,
  });

  return (
    <section className="payload-panel">
      <h3>{t("usage.failures.payloadHeading")}</h3>
      {!requested && (
        <>
          <p className="payload-note">{t("usage.failures.payloadWarning")}</p>
          <button type="button" className="button secondary" onClick={() => setRequested(true)}>
            {t("usage.failures.revealPayload")}
          </button>
        </>
      )}
      {requested && payload.isPending && <Loading />}
      {/* A miss here is the ordinary case, not a fault — capture may be off,
          the failure may predate it, or the record may have aged out — so it
          gets one short line and the reasons live in the Operator Guide. */}
      {requested && payload.isError && <p className="payload-note">{t("usage.failures.noPayload")}</p>}
      {requested && payload.data && (
        <>
          <PayloadSection
            label={t("usage.failures.payloadRequest")}
            value={payload.data.request}
            truncated={payload.data.request_truncated}
          />
          <PayloadSection
            label={t("usage.failures.payloadResponse")}
            value={payload.data.response}
            truncated={payload.data.response_truncated}
          />
        </>
      )}
    </section>
  );
}

function PayloadSection({ label, value, truncated }: { label: string; value: unknown; truncated?: boolean }) {
  const { t } = useTranslation();
  if (value === undefined || value === null) return null;
  return (
    <>
      <h4>{label}</h4>
      {/* Truncation is stated rather than left to be inferred: a reader who
          diagnoses a malformed body that is only an incomplete one goes looking
          for a bug the upstream does not have. */}
      {truncated && <p className="payload-truncated">{t("usage.failures.payloadTruncated")}</p>}
      <pre className="payload-body">{JSON.stringify(value, null, 2)}</pre>
    </>
  );
}
