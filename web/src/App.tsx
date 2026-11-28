import { Navigate, Route, Routes } from "react-router-dom";
import Layout from "./components/Layout";
import Dashboard from "./pages/Dashboard";
import Files from "./pages/Files";
import Tasks from "./pages/Tasks";
import Search from "./pages/Search";
import Chat from "./pages/Chat";
import Workflow from "./pages/Workflow";
import Sandbox from "./pages/Sandbox";
import Login from "./pages/Login";
import Users from "./pages/Users";
import SubAgents from "./pages/SubAgents";
import Approvals from "./pages/Approvals";
import ApiKeys from "./pages/ApiKeys";
import Webhooks from "./pages/Webhooks";

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<Layout />}>
        <Route index element={<Dashboard />} />
        <Route path="/files" element={<Files />} />
        <Route path="/tasks" element={<Tasks />} />
        <Route path="/search" element={<Search />} />
        <Route path="/chat" element={<Chat />} />
        <Route path="/workflow" element={<Workflow />} />
        <Route path="/approvals" element={<Approvals />} />
        <Route path="/sandbox" element={<Sandbox />} />
        <Route path="/subagents" element={<SubAgents />} />
        <Route path="/users" element={<Users />} />
        <Route path="/api-keys" element={<ApiKeys />} />
        <Route path="/webhooks" element={<Webhooks />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
