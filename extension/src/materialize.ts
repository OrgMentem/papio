// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

const BINDING_ID = /^[A-Za-z0-9_-]{8,128}$/;
const statusMarker = document.getElementById("materialize-status");

if (!(statusMarker instanceof HTMLElement)) {
  throw new Error("materialization status marker is missing");
}

const fragment = location.hash.startsWith("#") ? location.hash.slice(1) : "";
if (BINDING_ID.test(fragment)) {
  statusMarker.dataset.state = "valid";
  statusMarker.dataset.bindingId = fragment;
  statusMarker.textContent = "Materialization binding ready";
} else {
  statusMarker.dataset.state = "invalid";
  statusMarker.removeAttribute("data-binding-id");
  statusMarker.textContent = "Invalid materialization binding";
}
