(function () {
  function el(id) { return document.getElementById(id); }

  function esc(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
  }

  function render(snap) {
    if (!snap) {
      el("generated").textContent = "No snapshot. Run: darwin-node debug-dump -o debug-snapshot.json";
      return;
    }
    if (!snap.node || !snap.slots) {
      el("generated").textContent = "snapshot missing node/slots fields";
      return;
    }
    el("generated").textContent = "snapshot " + (snap.generatedAt || "") + "  (file:// safe, not an ES module)";
    var cap = snap.node.capacity || {};
    var capBits = [];
    for (var k in cap) {
      if (Object.prototype.hasOwnProperty.call(cap, k)) {
        capBits.push(esc(k) + "=" + esc(cap[k]));
      }
    }
    el("node").innerHTML =
      "<p><strong>" + esc(snap.node.name) + "</strong> runtime=" + esc(snap.node.runtime) +
      " kubelet=" + esc(snap.node.kubeletVersion) + "</p><p>" + capBits.join(" · ") + "</p>";
    var conds = snap.node.conditions || [];
    var ch = "";
    for (var i = 0; i < conds.length; i++) {
      var c = conds[i];
      var on = String(c.status).toLowerCase() === "true";
      ch += "<span class='pill " + (on ? "true" : "false") + "'>" + esc(c.type) + "=" + esc(c.status) + "</span>";
    }
    el("conditions").innerHTML = ch;
    var slots = el("slots");
    slots.innerHTML = "";
    var max = snap.slots.max || 0;
    var used = snap.slots.used || 0;
    for (var s = 0; s < max; s++) {
      var d = document.createElement("div");
      d.className = "slot " + (s < used ? "used" : "free");
      d.textContent = s < used ? "slot " + (s + 1) + " in use" : "slot " + (s + 1) + " free";
      slots.appendChild(d);
    }
    el("slot-uids").textContent = "uids: " + ((snap.slots.uids || []).join(", ") || "(none)") +
      "  used=" + used + " max=" + max + " free=" + (snap.slots.free != null ? snap.slots.free : (max - used));
    var pods = snap.pods || [];
    var tbody = el("pods");
    tbody.innerHTML = "";
    if (!pods.length) {
      tbody.innerHTML = "<tr><td colspan='8'>no pods</td></tr>";
    }
    for (var p = 0; p < pods.length; p++) {
      var pod = pods[p];
      var tr = document.createElement("tr");
      tr.innerHTML = "<td>" + esc(pod.namespace) + "/" + esc(pod.name) + "</td>" +
        "<td>" + esc(pod.phase) + "</td>" +
        "<td>" + (pod.ready ? "yes" : "no") + "</td>" +
        "<td>" + (pod.agentOK ? "yes" : "no") + "</td>" +
        "<td>" + esc(pod.vmState) + "</td>" +
        "<td>" + esc(pod.podIP) + "</td>" +
        "<td>" + esc(pod.restartCount) + "</td>" +
        "<td class='msg'>" + esc(pod.reason) + (pod.message ? (" / " + pod.message) : "") + "</td>";
      tbody.appendChild(tr);
    }
    var agents = snap.agents || [];
    var ab = el("agents");
    ab.innerHTML = "";
    if (!agents.length) {
      ab.innerHTML = "<tr><td colspan='3'>no agents</td></tr>";
    }
    for (var a = 0; a < agents.length; a++) {
      var ag = agents[a];
      var atr = document.createElement("tr");
      atr.innerHTML = "<td>" + esc(ag.pod) + "</td><td>" + (ag.ok ? "yes" : "no") + "</td><td>" + esc(ag.vmState) + "</td>";
      ab.appendChild(atr);
    }
    try {
      el("raw").textContent = JSON.stringify(snap, null, 2);
    } catch (e) {
      el("raw").textContent = String(e);
    }
  }

  function boot() {
    if (window.DARWIN_NODE_SNAPSHOT) {
      render(window.DARWIN_NODE_SNAPSHOT);
    } else {
      render(null);
    }
    var input = el("file");
    if (input) {
      input.addEventListener("change", function (ev) {
        var f = ev.target.files && ev.target.files[0];
        if (!f) return;
        var reader = new FileReader();
        reader.onload = function () {
          try {
            render(JSON.parse(String(reader.result)));
          } catch (e) {
            el("generated").textContent = "invalid snapshot: " + e;
          }
        };
        reader.readAsText(f);
      });
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
