import { Routes, Route, Navigate } from "react-router";
import { Layout } from "./components/Layout";
import { MapPage } from "./pages/Map";
import { JobsPage } from "./pages/Jobs";
import { JobDetailPage } from "./pages/JobDetail";
import { DispatchPage } from "./pages/Dispatch";
import { SettingsPage } from "./pages/Settings";

export function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/map" replace />} />
        <Route path="/map" element={<MapPage />} />
        {/* The old Fleet page is the map now; keep its links working. */}
        <Route path="/fleet" element={<Navigate to="/map" replace />} />
        <Route path="/jobs" element={<JobsPage />} />
        <Route path="/jobs/:id" element={<JobDetailPage />} />
        <Route path="/dispatch" element={<DispatchPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/map" replace />} />
      </Route>
    </Routes>
  );
}
