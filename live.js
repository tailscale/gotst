(function() {
    "use strict";

    var currentHTML = "";
    var ws = null;
    var stopped = false;
    var reconnectTimer = null;
    var sort = {key: "changed", direction: "desc"};

    function morph(live, fresh) {
        var liveChildren = live.childNodes;
        var freshChildren = fresh.childNodes;
        var i = 0;
        for (; i < freshChildren.length; i++) {
            if (i >= liveChildren.length) {
                live.appendChild(freshChildren[i].cloneNode(true));
                continue;
            }
            var oldNode = liveChildren[i];
            var newNode = freshChildren[i];
            if (oldNode.nodeType === 1 && newNode.nodeType === 1 && oldNode.tagName === newNode.tagName) {
                morphAttrs(oldNode, newNode);
                morph(oldNode, newNode);
            } else if (oldNode.nodeType === 3 && newNode.nodeType === 3) {
                if (oldNode.nodeValue !== newNode.nodeValue) oldNode.nodeValue = newNode.nodeValue;
            } else {
                live.replaceChild(newNode.cloneNode(true), oldNode);
            }
        }
        while (liveChildren.length > freshChildren.length) {
            live.removeChild(liveChildren[freshChildren.length]);
        }
    }

    function morphAttrs(live, fresh) {
        for (var i = live.attributes.length - 1; i >= 0; i--) {
            var name = live.attributes[i].name;
            if (live.tagName === "DETAILS" && name === "open") continue;
            if (!fresh.hasAttribute(name)) live.removeAttribute(name);
        }
        for (var j = 0; j < fresh.attributes.length; j++) {
            var attr = fresh.attributes[j];
            if (live.getAttribute(attr.name) !== attr.value) live.setAttribute(attr.name, attr.value);
        }
    }

    function setConnection(text, state) {
        var node = document.getElementById("connection-status");
        if (!node) return;
        node.textContent = text;
        node.className = "connection " + state;
        var stop = document.getElementById("stop-live");
        var start = document.getElementById("start-live");
        if (stop) stop.disabled = stopped;
        if (start) start.disabled = !stopped;
    }

    function applyPatch(patch) {
        var oldRunes = Array.from(currentHTML);
        var insertRunes = Array.from(patch.insert || "");
        var suffix = patch.suffix || 0;
        currentHTML = oldRunes.slice(0, patch.prefix)
            .concat(insertRunes)
            .concat(suffix ? oldRunes.slice(oldRunes.length - suffix) : [])
            .join("");
        var fresh = new DOMParser().parseFromString(currentHTML, "text/html");
        morph(document.body, fresh.body);
        bindControls();
        sortPackages();
        setConnection("Connected", "connected");
        if (patch.final && ws) ws.send(JSON.stringify({ack: patch.seq}));
    }

    function sortPackages() {
        var body = document.querySelector("#packages tbody");
        if (!body) return;
        var rows = Array.from(body.rows);
        rows.sort(function(a, b) {
            var av = a.dataset[sort.key] || "";
            var bv = b.dataset[sort.key] || "";
            if (sort.key === "tests" || sort.key === "changed") {
                av = Number(av); bv = Number(bv);
                return sort.direction === "asc" ? av - bv : bv - av;
            }
            var result = av.localeCompare(bv);
            return sort.direction === "asc" ? result : -result;
        });
        rows.forEach(function(row) { body.appendChild(row); });
        document.querySelectorAll("#packages th button").forEach(function(button) {
            if (button.dataset.sort === sort.key) button.dataset.direction = sort.direction;
            else delete button.dataset.direction;
        });
    }

    function bindControls() {
        var stop = document.getElementById("stop-live");
        if (stop) {
            stop.onclick = function() {
                stopped = true;
                if (reconnectTimer !== null) {
                    clearTimeout(reconnectTimer);
                    reconnectTimer = null;
                }
                if (ws) ws.close();
                setConnection("Stopped", "stopped");
            };
        }
        var start = document.getElementById("start-live");
        if (start) {
            start.onclick = function() {
                if (!stopped) return;
                stopped = false;
                connect();
            };
        }
        document.querySelectorAll("#packages th button").forEach(function(button) {
            button.onclick = function() {
                var key = button.dataset.sort;
                if (sort.key === key) sort.direction = sort.direction === "asc" ? "desc" : "asc";
                else sort = {key: key, direction: "asc"};
                sortPackages();
            };
        });
    }

    function connect() {
        if (stopped) return;
        if (reconnectTimer !== null) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
        currentHTML = "";
        setConnection("Reconnecting…", "connecting");
        var proto = window.location.protocol === "https:" ? "wss:" : "ws:";
        var socket = new WebSocket(proto + "//" + window.location.host + "/live-ws");
        ws = socket;
        socket.onopen = function() { setConnection("Connected", "connected"); };
        socket.onmessage = function(event) {
            try { applyPatch(JSON.parse(event.data)); } catch (_) {}
        };
        socket.onclose = function() {
            if (ws !== socket) return;
            ws = null;
            if (stopped) {
                setConnection("Stopped", "stopped");
                return;
            }
            setConnection("Reconnecting…", "connecting");
            reconnectTimer = setTimeout(connect, 1000);
        };
        socket.onerror = function() {
            setConnection("Disconnected", "disconnected");
            socket.close();
        };
    }

    bindControls();
    sortPackages();
    connect();
})();
