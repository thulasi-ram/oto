import { Route, Router } from "@solidjs/router";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import type { Component } from "solid-js";

import Index from "~/routes/index";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      retry: 1,
      refetchOnWindowFocus: true,
    },
  },
});

const App: Component = () => (
  <QueryClientProvider client={queryClient}>
    <Router>
      <Route path="/" component={Index} />
    </Router>
  </QueryClientProvider>
);

export default App;
