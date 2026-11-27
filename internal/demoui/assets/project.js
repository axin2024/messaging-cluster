import { $, feedback } from "./project-common.js";
import "./project-member.js";
import "./project-admin.js";

for (const mode of ["member", "admin"]) {
  $(`${mode}-mode`).addEventListener("click", () => {
    feedback();
    for (const other of ["member", "admin"]) {
      $(`${other}-view`).hidden = mode !== other;
      $(`${other}-mode`).setAttribute("aria-pressed", String(mode === other));
    }
  });
}
