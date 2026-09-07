import { Routes, Route, Navigate } from "react-router";
import { Layout } from "./components/Layout";
import { FleetPage } from "./pages/Fleet";
import { JobsPage } from "./pages/Jobs";
import { JobDetailPage } from "./pages/JobDetail";
import { DispatchPage } from "./pages/Dispatch";
import { SettingsPage } from "./pages/Settings";

export function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/fleet" replace />} />
        <Route path="/fleet" element={<FleetPage />} />
        <Route path="/jobs" element={<JobsPage />} />
        <Route path="/jobs/:id" element={<JobDetailPage />} />
        <Route path="/dispatch" element={<DispatchPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/fleet" replace />} />
      </Route>
    </Routes>
  );
}
