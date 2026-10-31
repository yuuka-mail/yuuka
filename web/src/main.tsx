import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { isUnauthenticated } from "@/api/client";
import { keys } from "@/api/queries";

import { App } from "./app";
import "./index.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A 401 means the session ended: show the login, don't retry.
      retry: (n, err) => !isUnauthenticated(err) && n < 2,
      refetchOnWindowFocus: false,
    },
  },
});

// Any API call that finds the session gone sends the shell back to login.
queryClient.getQueryCache().subscribe((e) => {
  if (e.type === "updated" && e.query.queryKey[1] !== keys.session[1] && isUnauthenticated(e.query.state.error)) {
    queryClient.setQueryData(keys.session, null);
  }
});

const root = document.getElementById("root");
if (!root) throw new Error("no #root");
createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
);
