import { Navigate, Route, Routes } from "react-router-dom";
import Layout from "./components/Layout";
import Dashboard from "./pages/Dashboard";
import Files from "./pages/Files";
import Tasks from "./pages/Tasks";
import Search from "./pages/Search";
import Workflow from "./pages/Workflow";
import Sandbox from "./pages/Sandbox";

export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Dashboard />} />
        <Route path="/files" element={<Files />} />
        <Route path="/tasks" element={<Tasks />} />
        <Route path="/search" element={<Search />} />
        <Route path="/workflow" element={<Workflow />} />
        <Route path="/sandbox" element={<Sandbox />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
