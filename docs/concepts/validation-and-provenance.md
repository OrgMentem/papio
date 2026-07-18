# Validation and provenance

*papio* does not treat a download URL as a finished file. It downloads candidates in
ranked order and quarantines each one before it can become part of a finished
job. This page describes the decision between that quarantine and a trusted,
exportable PDF. For the surrounding resolver and job flow, see the
[acquisition pipeline](../concepts/acquisition-pipeline.md).

## Validation gates

Validation has three gates. A candidate must clear each applicable gate; it is
not accepted merely because a server returned PDF-like bytes.

1. **Structural gate.** *papio* rejects anything that isn't a usable download,
   then checks the PDF's header and end marker, parses its structure and page
   count in an isolated worker under strict resource limits, and records whether it is
   encrypted or carries active content. A malformed, zero-page, encrypted/password-protected, active-JavaScript,
   or embedded-file PDF is never accepted. Encrypted and
   active-content PDFs are explicit rejections; a review decision cannot waive
   either one.
2. **Identity gate.** Extracted evidence is compared with the requested work,
   including title, author, year, and DOI evidence. A conflicting DOI or clearly
   different title or author is an explicit wrong-work rejection. *papio* does not
   accept the first plausible URL or substitute one work for another.
3. **Text gate.** *papio* uses Poppler's text extraction (capped by the `[pdf]` config) for the
   semantic evidence. When text is absent or too sparse, it renders only the
   first few pages (up to `max_ocr_pages`) with Poppler and runs Tesseract OCR, recording that
   OCR informed identity. OCR is a fallback for evidence, not a way to make an
   unverified paper pass.

The `[pdf]` configuration controls the text gate:

| Key | Effect |
| --- | --- |
| `ocr_enabled` | Enables the OCR fallback. When disabled, image-only papers require review. |
| `min_text_chars` | Extracted-text threshold before OCR fallback is relevant. |
| `max_ocr_pages` | Maximum number of pages processed by the OCR fallback. |
| `title_match_threshold` | Threshold for matching the PDF title to the requested work. |

See the [configuration reference](../reference/config-reference.md#pdf) for
all four settings, defaults, and constraints. In particular, enabled OCR
requires `pdftoppm` and `tesseract`; *papio* reports its structural, semantic, and
OCR capabilities separately.

## Review is a human decision

Evidence that is semantically or identity-ambiguous parks the job in
`needs_review` rather than silently accepting the PDF. The open
`verify_identity` action gives the local quarantine-file path. Inspect the file,
then resolve that specific action deliberately:

```sh
papio actions resolve <action-id> --accept
# or
papio actions resolve <action-id> --reject
```

`--accept` states that you verified the quarantined PDF is the requested work.
It requeues the candidate, and its identity result is recorded as
`user_confirmed`, not as a machine pass. `--reject` records that it is not the
requested work and cancels the review. Neither option can override an explicit
wrong-work, encrypted, or active-content rejection.

## From accepted PDF to bundle

After validation, *papio* saves the file as `artifacts/<sha256>.pdf`. Its name is
its content hash, so from that point the file is permanent and can't change
without detection.

A ready job exports an `AcquisitionBundle` directory:

```text
bundle.json
artifacts/<sha256>.pdf
```

`bundle.json` carries the schema and job/work identifiers; normalized
bibliographic identity and evidence; the selected candidate's source, version,
access basis, and reuse license; sanitized landing/source URLs and source record
IDs; retrieval and adapter timestamps and versions; the PDF's content hash, size,
detected MIME, page count, and text/OCR metrics; the validation and identity
decision; the relative file path; and a provenance event digest. It also
carries the zotio item key when the request originated with zotio.

**Access basis and reuse license are separate facts.** Access basis records the
basis on which *papio* acquired the candidate under the selected policy; reuse
license records license information for that candidate. A usable access path is
not proof of an open reuse license, so unknown copyright is never exported as an
open-license claim.

The bundle and saved state keep URLs sanitized. They never record API keys,
cookies, signed query values, login-page URLs, or browser credentials. This keeps
enough provenance to explain an accepted file without turning the bundle or
database into a store of access tokens.
