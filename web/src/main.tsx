import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "./api";
import { App } from "./App";
import { ErrorBoundary } from "./components";
import { NotificationProvider } from "./notifications";
import "./i18n";
import "./design-system/index.css";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: false,
      retry: (count, error) => !(error instanceof ApiError && error.status < 500) && count < 2,
    },
    mutations: { retry: false },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <NotificationProvider>
        <ErrorBoundary>
          <App />
        </ErrorBoundary>
      </NotificationProvider>
    </QueryClientProvider>
  </React.StrictMode>,
);
