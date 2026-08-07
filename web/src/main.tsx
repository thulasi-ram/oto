/* @refresh reload */
import { render } from "solid-js/web";

import App from "~/App";
import "~/index.css";

const root = document.getElementById("root");
if (!root) {
  throw new Error("oto: #root is missing from index.html");
}

render(() => <App />, root);
