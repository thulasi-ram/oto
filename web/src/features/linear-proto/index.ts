/*
 * The scoped stylesheet is not imported here (this is a .ts barrel; the route
 * mounting the prototype should import the CSS once, directly, e.g.:
 *   import "~/features/linear-proto/linear-proto.css";
 * alongside mounting a wrapper element carrying the `linear-proto` class.
 */
export * from "~/features/linear-proto/types";
export * from "~/features/linear-proto/layout";
export * from "~/features/linear-proto/store";
export * from "~/features/linear-proto/mockData";

export * from "~/features/linear-proto/primitives/PriorityIcon";
export * from "~/features/linear-proto/primitives/StatusIcon";
export * from "~/features/linear-proto/primitives/Avatar";
export * from "~/features/linear-proto/primitives/Keycap";
export * from "~/features/linear-proto/primitives/LabelBadge";
export * from "~/features/linear-proto/primitives/ProjectBadge";
export * from "~/features/linear-proto/primitives/Checkbox";
export * from "~/features/linear-proto/primitives/Menu";
export * from "~/features/linear-proto/primitives/Popover";
