const { createApp, ref, reactive, computed, watch, onMounted, onUnmounted, nextTick } = Vue;

function getAuthHeaders() {
  const stored = localStorage.getItem("admin_token");
  if (!stored) return {};
  return { Authorization: "Bearer " + stored };
}

// Chart Manager
const chartInstances = {};
function destroyChart(name) {
  if (chartInstances[name]) { try { chartInstances[name].destroy(); } catch {} delete chartInstances[name]; }
}
function createChart(canvasId, config) {
  destroyChart(canvasId);
  const canvas = document.getElementById(canvasId);
  if (!canvas) return;
  try { chartInstances[canvasId] = new Chart(canvas.getContext("2d"), config); } catch (err) { console.error("Chart creation failed:", canvasId, err); }
}

function formatNumber(n) {
  if (n === 0 || n == null) return "0";
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(1) + "B";
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return n.toString();
}

function daysAgo(n) {
  const d = new Date(); d.setDate(d.getDate() - n); return d.toISOString().slice(0, 10);
}

function buildBarData(labels, values, label, color) {
  return {
    type: "bar",
    data: { labels, datasets: [{ label, data: values, backgroundColor: color, borderRadius: 4 }] },
    options: {
      responsive: true, maintainAspectRatio: false,
      plugins: {
        legend: { display: false },
        tooltip: { backgroundColor: "#1c2333", titleColor: "#c9d1d9", bodyColor: "#8b949e", borderColor: "#30363d", borderWidth: 1 },
      },
      scales: {
        x: { ticks: { color: "#8b949e" }, grid: { color: "#30363d" } },
        y: { ticks: { color: "#8b949e", callback: formatNumber }, grid: { color: "#30363d" } },
      },
    },
  };
}

function buildPieData(labels, values) {
  const colors = ["#58a6ff", "#3fb950", "#d29922", "#f85149", "#bc8cff", "#79c0ff", "#56d364", "#e3b341", "#ff7b72"];
  return {
    type: "doughnut",
    data: { labels, datasets: [{ data: values, backgroundColor: colors.slice(0, values.length), borderWidth: 0 }] },
    options: {
      responsive: true, maintainAspectRatio: false,
      plugins: {
        legend: { position: "right", labels: { color: "#8b949e", font: { size: 11 }, padding: 8 } },
        tooltip: { backgroundColor: "#1c2333", titleColor: "#c9d1d9", bodyColor: "#8b949e", borderColor: "#30363d", borderWidth: 1 },
      },
    },
  };
}

// ===== App =====
const app = createApp({
  setup() {
    const currentView = ref("overview");
    const showAddKeyModal = ref(false);
    const showAddModelModal = ref(false);
    const appVersion = ref("");
    const appEnv = ref("");
    const isAuthenticated = ref(!!localStorage.getItem("admin_token"));
    const loginPassword = ref("");
    const loginError = ref("");
    const loggingIn = ref(false);

    async function apiFetch(url) {
      try {
        const stored = localStorage.getItem("admin_token");
        const headers = stored ? { Authorization: "Bearer " + stored } : {};
        const res = await fetch(url, { headers });
        if (res.status === 401) {
          localStorage.removeItem("admin_token");
          isAuthenticated.value = false;
          return null;
        }
        if (!res.ok) { console.error("API error:", url, res.status); return null; }
        return res.json();
      } catch (err) { console.error("Fetch error:", err); return null; }
    }

    const overview = reactive({ totalRequests: 0, totalTokens: 0, totalInput: 0, totalOutput: 0 });
    const usageGranularity = ref("daily");
    const selectedUsageKey = ref("");
    const usageStatsData = ref([]);
    const apiKeys = ref([]);
    const newKeyName = ref("");
    const newKeyValue = ref("");
    const newModelName = ref("");
    const newModelTier = ref("");
    const breakerStates = reactive({});
    const virtualModels = ref([]);
    const realModelConfig = reactive({ strategy: "", models: [] });
    const appConfig = reactive({ name: "", version: "", env: "", port: 8080 });
    const showAddRealModelModal = ref(false);
    const realModelEditIndex = ref(-1);
    const realModelForm = reactive({ provider: "", model: "", weight: 1, tier: "", cost: 0, timeout: 3000, disabled: false });
    const providerNames = computed(() => { const keys = Object.keys(providersConfig); return keys.length ? keys : ["seneenova", "seneenova_me", "deepseek_openai", "deepseek_anthropic", "openai", "anthropic", "xiaomi_tp", "glm", "nvidia"]; });
    const providersConfig = reactive({});
    const showAddProviderModal = ref(false);
    const providerEditName = ref("");
    const providerForm = reactive({ name: "", base_url: "", api_key: "", protocol: "openai", timeout: 3000 });

    const navItems = [
      { id: "overview", icon: "📊", label: "概览" },
      { id: "usage", icon: "🔢", label: "Token 用量" },
      { id: "apikeys", icon: "🔑", label: "API Key" },
      { id: "providers", icon: "🔌", label: "Provider 监控" },
            { id: "config", icon: "⚙️", label: "配置" },
    ];

    async function login() {
      loginError.value = "";
      if (!loginPassword.value) { loginError.value = "请输入密码"; return; }
      loggingIn.value = true;
      try {
        const res = await fetch("/admin/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password: loginPassword.value }),
        });
        const data = await res.json();
        if (res.ok && data.token) {
          localStorage.setItem("admin_token", data.token);
          isAuthenticated.value = true;
          loginPassword.value = "";
          await loadOverview();
          await loadConfig();
        } else {
          loginError.value = data.error || "登录失败";
        }
      } catch (err) {
        loginError.value = "登录失败: " + err.message;
      } finally {
        loggingIn.value = false;
      }
    }

    async function loadOverview() {
      const [stats, daily, realModels] = await Promise.all([
        apiFetch("/admin/usage/stats"),
        apiFetch("/admin/usage/daily?start_time=" + daysAgo(30)),
        apiFetch("/admin/usage/by-real-model"),
      ]);
      if (stats?.data) {
        overview.totalRequests = stats.data.total_requests || 0;
        overview.totalTokens = stats.data.total_tokens || 0;
        overview.totalInput = stats.data.total_input || 0;
        overview.totalOutput = stats.data.total_output || 0;
      }
      if (daily?.data) {
        const grouped = {};
        daily.data.forEach((d) => { if (!grouped[d.date]) grouped[d.date] = 0; grouped[d.date] += d.total_tokens || 0; });
        const days = Object.keys(grouped).sort();
        createChart("dailyChart", buildBarData(days, days.map((d) => grouped[d]), "Token 消耗", "#58a6ff"));
      }
      if (realModels?.data) {
        createChart("modelChart", buildPieData(realModels.data.map((d) => d.model || d.date), realModels.data.map((d) => d.total_tokens || 0)));
      }
    }

    async function loadUsageStats() {
      let url = "/admin/usage/daily?start_time=" + daysAgo(30) + "&granularity=" + usageGranularity.value;
      if (selectedUsageKey.value) {
        url = "/admin/usage/by-api-key?api_key=" + encodeURIComponent(selectedUsageKey.value) + "&granularity=" + usageGranularity.value + "&start_time=" + daysAgo(30);
      }
      const res = await apiFetch(url);
      if (!res?.data) { usageStatsData.value = []; return; }
      usageStatsData.value = res.data;
      const labels = res.data.map((d) => d.date + " " + d.model);
      createChart("usageChart", {
        type: "bar",
        data: {
          labels,
          datasets: [
            { label: "输入", data: res.data.map((d) => d.total_input || 0), backgroundColor: "#58a6ff", borderRadius: 4 },
            { label: "输出", data: res.data.map((d) => d.total_output || 0), backgroundColor: "#3fb950", borderRadius: 4 },
          ],
        },
        options: {
          responsive: true, maintainAspectRatio: false,
          plugins: {
            legend: { labels: { color: "#8b949e" } },
            tooltip: { backgroundColor: "#1c2333", titleColor: "#c9d1d9", bodyColor: "#8b949e", borderColor: "#30363d", borderWidth: 1, callbacks: { label: (ctx) => ctx.dataset.label + ": " + formatNumber(ctx.raw) } },
          },
          scales: {
            x: { ticks: { color: "#8b949e", maxTicksLimit: 15 }, grid: { color: "#30363d" } },
            y: { ticks: { color: "#8b949e", callback: formatNumber }, grid: { color: "#30363d" } },
          },
        },
      });
    }

    async function loadByRealModelChart() {
      const res = await apiFetch("/admin/usage/by-real-model");
      if (!res?.data) return;
      createChart("byRealModelChart", buildBarData(res.data.map((d) => d.model), res.data.map((d) => d.total_tokens || 0), "Token 消耗", "#58a6ff"));
    }

    async function loadAPIKeys() { const res = await apiFetch("/admin/api-keys"); if (res?.data) apiKeys.value = res.data; }
    async function loadBreakers() { const res = await apiFetch("/admin/providers"); if (res?.breaker_states) Object.assign(breakerStates, res.breaker_states); }
    

    async function loadConfig() {
      const [cfg, models, realModels, providers] = await Promise.all([
        apiFetch("/admin/config"), apiFetch("/admin/models"),
        apiFetch("/admin/real-models"), apiFetch("/admin/providers/config"),
      ]);
      if (cfg?.app) { Object.assign(appConfig, cfg.app); appVersion.value = cfg.app.version || ""; appEnv.value = cfg.app.env || ""; }
      if (models?.data) virtualModels.value = models.data;
      if (realModels) Object.assign(realModelConfig, realModels);
      if (providers?.data) Object.assign(providersConfig, providers.data);
    }

    async function addKey() {
      if (!newKeyName.value || !newKeyValue.value) { alert("请填写名称和 API Key"); return; }
      const res = await fetch("/admin/api-keys", { method: "POST", headers: { "Content-Type": "application/json", ...getAuthHeaders() }, body: JSON.stringify({ name: newKeyName.value, key: newKeyValue.value }) });
      if (res.status === 201) { showAddKeyModal.value = false; newKeyName.value = ""; newKeyValue.value = ""; await loadAPIKeys(); } else alert("创建失败");
    }

    async function deleteKey(key) {
      if (!confirm("确定删除这个 API Key 吗？")) return;
      const res = await fetch("/admin/api-keys/" + encodeURIComponent(key), { method: "DELETE", headers: getAuthHeaders() });
      if (res.status === 200) await loadAPIKeys(); else alert("删除失败");
    }

    async function addModel() {
      if (!newModelName.value) { alert("请填写模型名"); return; }
      const res = await fetch("/admin/models", { method: "POST", headers: { "Content-Type": "application/json", ...getAuthHeaders() }, body: JSON.stringify({ name: newModelName.value, tier: newModelTier.value }) });
      if (res.status === 201) { showAddModelModal.value = false; newModelName.value = ""; newModelTier.value = ""; await loadConfig(); } else alert("创建失败");
    }

    async function deleteModel(name) {
      if (!confirm("确定删除虚拟模型 " + name + " 吗？")) return;
      const res = await fetch("/admin/models/" + encodeURIComponent(name), { method: "DELETE", headers: getAuthHeaders() });
      if (res.status === 200) await loadConfig(); else alert("删除失败");
    }

    async function updateStrategy() {
      const res = await fetch("/admin/real-models/strategy", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", ...getAuthHeaders() },
        body: JSON.stringify({ strategy: realModelConfig.strategy }),
      });
      if (!res.ok) {
        alert("更新策略失败");
        await loadConfig();
      }
    }

    function editRealModel(index) {
      const m = realModelConfig.models[index];
      if (!m) return;
      realModelEditIndex.value = index;
      realModelForm.provider = m.provider;
      realModelForm.model = m.model;
      realModelForm.weight = m.weight || 1;
      realModelForm.tier = m.tier || "";
      realModelForm.cost = m.cost || 0;
      realModelForm.timeout = m.timeout ? Math.floor(m.timeout / 1000) : 3000;
      realModelForm.disabled = !!m.disabled;
      showAddRealModelModal.value = true;
    }

    function closeRealModelModal() {
      showAddRealModelModal.value = false;
      realModelEditIndex.value = -1;
      realModelForm.provider = "";
      realModelForm.model = "";
      realModelForm.weight = 1;
      realModelForm.tier = "";
      realModelForm.cost = 0;
      realModelForm.timeout = 3000;
      realModelForm.disabled = false;
    }

    async function saveRealModel() {
      if (!realModelForm.provider || !realModelForm.model) {
        alert("请填写 Provider 和 Model");
        return;
      }
      const body = {
        provider: realModelForm.provider,
        model: realModelForm.model,
        weight: realModelForm.weight || 1,
        tier: realModelForm.tier || "",
        cost: realModelForm.cost || 0,
        timeout: (realModelForm.timeout || 3000) + "s",
        disabled: realModelForm.disabled,
      };

      let url, method, successStatus;
      if (realModelEditIndex.value === -1) {
        url = "/admin/real-models";
        method = "POST";
        successStatus = 201;
      } else {
        url = "/admin/real-models/" + realModelEditIndex.value;
        method = "PUT";
        successStatus = 200;
      }

      const res = await fetch(url, {
        method: method,
        headers: { "Content-Type": "application/json", ...getAuthHeaders() },
        body: JSON.stringify(body),
      });
      if (res.status === successStatus) {
        closeRealModelModal();
        await loadConfig();
      } else {
        const err = await res.json().catch(() => ({}));
        alert("操作失败: " + (err.error || res.statusText));
      }
    }

    async function deleteRealModel(index) {
      if (!confirm("确定删除这条路由配置吗？")) return;
      const res = await fetch("/admin/real-models/" + index, {
        method: "DELETE",
        headers: getAuthHeaders(),
      });
      if (res.status === 200) {
        await loadConfig();
      } else {
        alert("删除失败");
      }
    }

    function editProvider(name) {
      const p = providersConfig[name];
      if (!p) return;
      providerEditName.value = name;
      providerForm.name = name;
      providerForm.base_url = p.base_url;
      providerForm.api_key = "";
      providerForm.protocol = p.protocol || "openai";
      providerForm.timeout = parseTimeout(p.timeout) || 3000;
      showAddProviderModal.value = true;
    }

    function closeProviderModal() {
      showAddProviderModal.value = false;
      providerEditName.value = "";
      providerForm.name = "";
      providerForm.base_url = "";
      providerForm.api_key = "";
      providerForm.protocol = "openai";
      providerForm.timeout = 3000;
    }

    function parseTimeout(s) {
      if (!s) return 3000;
      const m = s.match(/^(\d+)s$/);
      if (m) return parseInt(m[1]);
      const m2 = s.match(/^(\d+)m0s$/);
      if (m2) return parseInt(m2[1]) * 60;
      return 3000;
    }

    async function saveProvider() {
      if (!providerForm.name || !providerForm.base_url) {
        alert("请填写名称和 Base URL");
        return;
      }
      const body = {
        base_url: providerForm.base_url,
        api_key: providerForm.api_key || "",
        protocol: providerForm.protocol,
        timeout: (providerForm.timeout || 3000) + "s",
      };
      let url, method, successStatus;
      if (providerEditName.value) {
        url = "/admin/providers/" + encodeURIComponent(providerEditName.value);
        method = "PUT";
        successStatus = 200;
      } else {
        body.name = providerForm.name;
        url = "/admin/providers";
        method = "POST";
        successStatus = 201;
      }
      const res = await fetch(url, {
        method: method,
        headers: { "Content-Type": "application/json", ...getAuthHeaders() },
        body: JSON.stringify(body),
      });
      if (res.status === successStatus) {
        closeProviderModal();
        await loadConfig();
      } else {
        const err = await res.json().catch(() => ({}));
        alert("操作失败: " + (err.error || res.statusText));
      }
    }

    async function deleteProvider(name) {
      if (!confirm("确定删除 Provider " + name + " 吗？")) return;
      const res = await fetch("/admin/providers/" + encodeURIComponent(name), {
        method: "DELETE",
        headers: getAuthHeaders(),
      });
      if (res.status === 200) {
        await loadConfig();
      } else {
        alert("删除失败");
      }
    }

    function switchView(id) {
      currentView.value = id;
      nextTick(() => {
        if (id === "overview") loadOverview();
        else if (id === "usage") { loadAPIKeys(); loadUsageStats(); loadByRealModelChart(); }
        else if (id === "apikeys") loadAPIKeys();
        else if (id === "providers") loadBreakers();
        else if (id === "config") loadConfig();
      });
    }

    function formatDate(iso) { if (!iso) return ""; try { return new Date(iso).toLocaleString("zh-CN"); } catch { return iso; } }
    function maskKey(key) { if (!key || key.length <= 12) return key; return key.slice(0, 8) + "..." + key.slice(-4); }
    function copyKey(key) { navigator.clipboard.writeText(key).then(() => { const toast = document.createElement('div'); toast.textContent = '已复制到剪贴板'; toast.style.cssText = 'position:fixed;top:20px;left:50%;transform:translateX(-50%);background:var(--accent);color:#fff;padding:8px 16px;border-radius:4px;font-size:13px;z-index:9999;animation:fadeIn 0.3s ease;'; document.body.appendChild(toast); setTimeout(() => toast.remove(), 2000); }); }
    function getKeyName(key) { const k = apiKeys.value.find(k => k.key === key); return k ? k.name : (key || "—"); }

    function logout() {
      localStorage.removeItem("admin_token");
      isAuthenticated.value = false;
      loginPassword.value = "";
      loginError.value = "";
    }

    onMounted(async () => {
      if (isAuthenticated.value) {
        await loadOverview();
        await loadConfig();
      }
      const timer = setInterval(async () => {
        if (!isAuthenticated.value) return;
        if (currentView.value === "overview") await loadOverview();
        else if (currentView.value === "usage") await loadUsageStats();
        else if (currentView.value === "providers") await loadBreakers();
      }, 30000);
      onUnmounted(() => clearInterval(timer));
    });

    return {
      currentView, showAddKeyModal, showAddModelModal, appVersion, appEnv,
      isAuthenticated, loginPassword, loginError, loggingIn, login, logout,
      navItems, overview, usageGranularity, selectedUsageKey, usageStatsData, getKeyName,
      apiKeys, newKeyName, newKeyValue, newModelName, newModelTier,
      breakerStates, virtualModels, realModelConfig, appConfig,
      switchView, refreshAll: () => nextTick(loadOverview),
      loadUsageStats, addKey, deleteKey, addModel, deleteModel, updateStrategy,
      showAddRealModelModal, realModelEditIndex, realModelForm, providerNames,
      editRealModel, closeRealModelModal, saveRealModel, deleteRealModel,
      providersConfig, showAddProviderModal, providerEditName, providerForm,
      editProvider, closeProviderModal, saveProvider, deleteProvider,
      maskKey, copyKey, formatDate, formatNumber,
      fmt: formatNumber,
      fixed: (n, d) => (n == null ? "" : n.toFixed(d || 2)),
    };
  },
});

app.mount("#app");