// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

/** Result of inspecting an institutional resolver page. */
export type ResolverRoute =
  | { kind: "routed"; service: string }
  | { kind: "no_service" };

/**
 * Follow the first electronic service selected by the institution's resolver.
 *
 * Alma/Primo orders these links according to the institution's Online Services
 * configuration; this reproduces its direct-linking choice when the institution
 * has disabled automatic direct linking. It accepts only same-origin Alma
 * resolveService links and is injected only into a broker-owned tracked tab.
 * Passing a Document keeps the function pure for fixtures; passing null uses the
 * live page and navigates the same tracked tab so job correlation survives.
 */
export async function routeResolverService(
  input: Document | null,
  renderWaitMs?: number,
): Promise<ResolverRoute> {
  const page = input ?? document;
  const view = page.defaultView ?? window;
  const pageURL = new URL(page.location?.href ?? view.location.href);
  const firstService = (): HTMLAnchorElement | undefined =>
    Array.from(page.querySelectorAll<HTMLAnchorElement>("a[href]")).find((anchor) => {
      try {
        const target = new URL(anchor.href, pageURL);
        return (
          target.protocol === "https:" &&
          target.origin === pageURL.origin &&
          /\/view\/action\/uresolver\.do$/i.test(target.pathname) &&
          target.searchParams.get("operation") === "resolveService"
        );
      } catch {
        return false;
      }
    });

  let selected = firstService();
  const waitMs = renderWaitMs ?? (input === null ? 12_000 : 0);
  if (!selected && waitMs > 0) {
    // Primo NDE reports the tab as complete before Angular renders View Online.
    // Poll the small link set rather than racing that asynchronous service list;
    // polling also covers framework updates that do not emit useful mutations.
    selected = await new Promise<HTMLAnchorElement | undefined>((resolve) => {
      let settled = false;
      const finish = (candidate: HTMLAnchorElement | undefined): void => {
        if (settled) return;
        settled = true;
        view.clearInterval(interval);
        view.clearTimeout(timer);
        resolve(candidate);
      };
      const interval = view.setInterval(() => {
        const candidate = firstService();
        if (candidate) finish(candidate);
      }, Math.min(100, waitMs));
      const timer = view.setTimeout(() => finish(undefined), waitMs);
    });
  }
  if (!selected) return { kind: "no_service" };

  const service = (selected.getAttribute("aria-label") ?? selected.textContent ?? "full text")
    .replace(/\s+/g, " ")
    .trim()
    .slice(0, 160);
  if (input === null) view.location.assign(selected.href);
  return { kind: "routed", service };
}
