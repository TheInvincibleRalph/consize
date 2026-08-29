"use client";

import React from "react";
import WorkloadDetailView from "@/components/views/WorkloadDetailView";

export default function WorkloadDetailPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = React.use(params);
  return <WorkloadDetailView id={id} />;
}
