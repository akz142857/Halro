import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Layout } from "./Layout";
import { adoptTimeContext, resetAccountingTimeZone } from "./timezone";
import { timeContext } from "./test/fixtures";

function renderLayout() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <Layout username="admin">
        <div>content</div>
      </Layout>
    </QueryClientProvider>,
  );
}

describe("Layout", () => {
  afterEach(() => resetAccountingTimeZone());

  // Every figure in the console is measured against the accounting zone, so it
  // belongs somewhere always visible rather than on the one page that happens
  // to report a daily total.
  it("names the accounting time zone in the header", () => {
    adoptTimeContext(timeContext({ accounting_timezone: "Asia/Shanghai" }));
    renderLayout();
    expect(screen.getByText("Asia/Shanghai")).toBeInTheDocument();
    expect(screen.getByText(/本地控制 \| 无云端依赖/)).toBeInTheDocument();
  });

  it("follows the zone the server reports rather than a fixed default", () => {
    adoptTimeContext(timeContext({ accounting_timezone: "America/New_York" }));
    renderLayout();
    expect(screen.getByText("America/New_York")).toBeInTheDocument();
    expect(screen.queryByText("UTC")).not.toBeInTheDocument();
  });
});
